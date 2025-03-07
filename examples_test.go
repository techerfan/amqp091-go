// Copyright (c) 2021 VMware, Inc. or its affiliates. All Rights Reserved.
// Copyright (c) 2012-2021, Sean Treadway, SoundCloud Ltd.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package amqp091_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log"
	"net"
	"os"
	"runtime"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

func ExampleConfig_timeout() {
	// Provide your own anonymous Dial function that delegates to net.DialTimout
	// for custom timeouts

	conn, err := amqp.DialConfig("amqp:///", amqp.Config{
		Dial: func(network, addr string) (net.Conn, error) {
			return net.DialTimeout(network, addr, 2*time.Second)
		},
	})

	log.Printf("conn: %v, err: %v", conn, err)
}

func ExampleDialTLS() {
	// This example assume you have a RabbitMQ node running on localhost
	// with TLS enabled.
	//
	// The easiest way to create the CA, certificates and keys required for these
	// examples is by using tls-gen: https://github.com/michaelklishin/tls-gen
	//
	// A comprehensive RabbitMQ TLS guide can be found at
	// http://www.rabbitmq.com/ssl.html
	//
	// Once you have the required TLS files in place, use the following
	// rabbitmq.config example for the RabbitMQ node that you will run on
	// localhost:
	//
	//   [
	//   {rabbit, [
	//     {tcp_listeners, []},     % listens on 127.0.0.1:5672
	//     {ssl_listeners, [5671]}, % listens on 0.0.0.0:5671
	//     {ssl_options, [{cacertfile,"/path/to/your/testca/cacert.pem"},
	//                    {certfile,"/path/to/your/server/cert.pem"},
	//                    {keyfile,"/path/to/your/server/key.pem"},
	//                    {verify,verify_peer},
	//                    {fail_if_no_peer_cert,true}]}
	//     ]}
	//   ].
	//
	//
	// In the above rabbitmq.config example, we are disabling the plain AMQP port
	// and verifying that clients and fail if no certificate is presented.
	//
	// The self-signing certificate authority's certificate (cacert.pem) must be
	// included in the RootCAs to be trusted, otherwise the server certificate
	// will fail certificate verification.
	//
	// Alternatively to adding it to the tls.Config. you can add the CA's cert to
	// your system's root CAs.  The tls package will use the system roots
	// specific to each support OS.  Under OS X, add (drag/drop) cacert.pem
	// file to the 'Certificates' section of KeyChain.app to add and always
	// trust.  You can also add it via the command line:
	//
	//   security add-certificate testca/cacert.pem
	//   security add-trusted-cert testca/cacert.pem
	//
	// If you depend on the system root CAs, then use nil for the RootCAs field
	// so the system roots will be loaded instead.
	//
	// Server names are validated by the crypto/tls package, so the server
	// certificate must be made for the hostname in the URL.  Find the commonName
	// (CN) and make sure the hostname in the URL matches this common name.  Per
	// the RabbitMQ instructions (or tls-gen) for a self-signed cert, this defaults to the
	// current hostname.
	//
	//   openssl x509 -noout -in /path/to/certificate.pem -subject
	//
	// If your server name in your certificate is different than the host you are
	// connecting to, set the hostname used for verification in
	// ServerName field of the tls.Config struct.
	cfg := new(tls.Config)

	// see at the top
	cfg.RootCAs = x509.NewCertPool()

	if ca, err := os.ReadFile("testca/cacert.pem"); err == nil {
		cfg.RootCAs.AppendCertsFromPEM(ca)
	}

	// Move the client cert and key to a location specific to your application
	// and load them here.

	if cert, err := tls.LoadX509KeyPair("client/cert.pem", "client/key.pem"); err == nil {
		cfg.Certificates = append(cfg.Certificates, cert)
	}

	// see a note about Common Name (CN) at the top
	conn, err := amqp.DialTLS("amqps://server-name-from-certificate/", cfg)

	log.Printf("conn: %v, err: %v", conn, err)
}

func ExampleChannel_Confirm_bridge() {
	// This example acts as a bridge, shoveling all messages sent from the source
	// exchange "log" to destination exchange "log".

	// Confirming publishes can help from overproduction and ensure every message
	// is delivered.

	// Setup the source of the store and forward
	source, err := amqp.Dial("amqp://source/")
	if err != nil {
		log.Fatalf("connection.open source: %s", err)
	}
	defer source.Close()

	chs, err := source.Channel()
	if err != nil {
		log.Fatalf("channel.open source: %s", err)
	}

	if err := chs.ExchangeDeclare("log", amqp.Topic, true, false, false, false, nil); err != nil {
		log.Fatalf("exchange.declare destination: %s", err)
	}

	if _, err := chs.QueueDeclare("remote-tee", true, true, false, false, nil); err != nil {
		log.Fatalf("queue.declare source: %s", err)
	}

	if err := chs.QueueBind("remote-tee", "#", "logs", false, nil); err != nil {
		log.Fatalf("queue.bind source: %s", err)
	}

	shovel, err := chs.Consume("remote-tee", "shovel", false, false, false, false, nil)
	if err != nil {
		log.Fatalf("basic.consume source: %s", err)
	}

	// Setup the destination of the store and forward
	destination, err := amqp.Dial("amqp://destination/")
	if err != nil {
		log.Fatalf("connection.open destination: %s", err)
	}
	defer destination.Close()

	chd, err := destination.Channel()
	if err != nil {
		log.Fatalf("channel.open destination: %s", err)
	}

	if err := chd.ExchangeDeclare("log", amqp.Topic, true, false, false, false, nil); err != nil {
		log.Fatalf("exchange.declare destination: %s", err)
	}

	// Buffer of 1 for our single outstanding publishing
	confirms := chd.NotifyPublish(make(chan amqp.Confirmation, 1))

	if err := chd.Confirm(false); err != nil {
		log.Fatalf("confirm.select destination: %s", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Now pump the messages, one by one, a smarter implementation
	// would batch the deliveries and use multiple ack/nacks
	for {
		msg, ok := <-shovel
		if !ok {
			log.Fatalf("source channel closed, see the reconnect example for handling this")
		}

		err = chd.PublishWithContext(ctx, "logs", msg.RoutingKey, false, false, amqp.Publishing{
			// Copy all the properties
			ContentType:     msg.ContentType,
			ContentEncoding: msg.ContentEncoding,
			DeliveryMode:    msg.DeliveryMode,
			Priority:        msg.Priority,
			CorrelationId:   msg.CorrelationId,
			ReplyTo:         msg.ReplyTo,
			Expiration:      msg.Expiration,
			MessageId:       msg.MessageId,
			Timestamp:       msg.Timestamp,
			Type:            msg.Type,
			UserId:          msg.UserId,
			AppId:           msg.AppId,

			// Custom headers
			Headers: msg.Headers,

			// And the body
			Body: msg.Body,
		})
		if err != nil {
			if e := msg.Nack(false, false); e != nil {
				log.Printf("nack error: %+v", e)
			}
			log.Fatalf("basic.publish destination: %+v", msg)
		}

		// only ack the source delivery when the destination acks the publishing
		if confirmed := <-confirms; confirmed.Ack {
			if e := msg.Ack(false); e != nil {
				log.Printf("ack error: %+v", e)
			}
		} else {
			if e := msg.Nack(false, false); e != nil {
				log.Printf("nack error: %+v", e)
			}
		}
	}
}

func ExampleChannel_Consume() {
	// Connects opens an AMQP connection from the credentials in the URL.
	conn, err := amqp.Dial("amqp://guest:guest@localhost:5672/")
	if err != nil {
		log.Fatalf("connection.open: %s", err)
	}
	defer conn.Close()

	c, err := conn.Channel()
	if err != nil {
		log.Fatalf("channel.open: %s", err)
	}

	// We declare our topology on both the publisher and consumer to ensure they
	// are the same.  This is part of AMQP being a programmable messaging model.
	//
	// See the Channel.Publish example for the complimentary declare.
	err = c.ExchangeDeclare("logs", amqp.Topic, true, false, false, false, nil)
	if err != nil {
		log.Fatalf("exchange.declare: %s", err)
	}

	// Establish our queue topologies that we are responsible for
	type bind struct {
		queue string
		key   string
	}

	bindings := []bind{
		{"page", "alert"},
		{"email", "info"},
		{"firehose", "#"},
	}

	for _, b := range bindings {
		_, err = c.QueueDeclare(b.queue, true, false, false, false, nil)
		if err != nil {
			log.Fatalf("queue.declare: %v", err)
		}

		err = c.QueueBind(b.queue, b.key, "logs", false, nil)
		if err != nil {
			log.Fatalf("queue.bind: %v", err)
		}
	}

	// Set our quality of service.  Since we're sharing 3 consumers on the same
	// channel, we want at least 3 messages in flight.
	err = c.Qos(3, 0, false)
	if err != nil {
		log.Fatalf("basic.qos: %v", err)
	}

	// Establish our consumers that have different responsibilities.  Our first
	// two queues do not ack the messages on the server, so require to be acked
	// on the client.

	pages, err := c.Consume("page", "pager", false, false, false, false, nil)
	if err != nil {
		log.Fatalf("basic.consume: %v", err)
	}

	go func() {
		for page := range pages {
			// ... this consumer is responsible for sending pages per log
			if e := page.Ack(false); e != nil {
				log.Printf("ack error: %+v", e)
			}
		}
	}()

	// Notice how the concern for which messages arrive here are in the AMQP
	// topology and not in the queue.  We let the server pick a consumer tag this
	// time.

	emails, err := c.Consume("email", "", false, false, false, false, nil)
	if err != nil {
		log.Fatalf("basic.consume: %v", err)
	}

	go func() {
		for email := range emails {
			// ... this consumer is responsible for sending emails per log
			if e := email.Ack(false); e != nil {
				log.Printf("ack error: %+v", e)
			}
		}
	}()

	// This consumer requests that every message is acknowledged as soon as it's
	// delivered.

	firehose, err := c.Consume("firehose", "", true, false, false, false, nil)
	if err != nil {
		log.Fatalf("basic.consume: %v", err)
	}

	// To show how to process the items in parallel, we'll use a work pool.
	for i := 0; i < runtime.NumCPU(); i++ {
		go func(work <-chan amqp.Delivery) {
			for range work {
				// ... this consumer pulls from the firehose and doesn't need to acknowledge
			}
		}(firehose)
	}

	// Wait until you're ready to finish, could be a signal handler here.
	time.Sleep(10 * time.Second)

	// Cancelling a consumer by name will finish the range and gracefully end the
	// goroutine
	err = c.Cancel("pager", false)
	if err != nil {
		log.Fatalf("basic.cancel: %v", err)
	}

	// deferred closing the Connection will also finish the consumer's ranges of
	// their delivery chans.  If you need every delivery to be processed, make
	// sure to wait for all consumers goroutines to finish before exiting your
	// process.
}

func ExampleChannel_PublishWithContext() {
	// Connects opens an AMQP connection from the credentials in the URL.
	conn, err := amqp.Dial("amqp://guest:guest@localhost:5672/")
	if err != nil {
		log.Fatalf("connection.open: %s", err)
	}

	// This waits for a server acknowledgment which means the sockets will have
	// flushed all outbound publishings prior to returning.  It's important to
	// block on Close to not lose any publishings.
	defer conn.Close()

	c, err := conn.Channel()
	if err != nil {
		log.Fatalf("channel.open: %s", err)
	}

	// We declare our topology on both the publisher and consumer to ensure they
	// are the same.  This is part of AMQP being a programmable messaging model.
	//
	// See the Channel.Consume example for the complimentary declare.
	err = c.ExchangeDeclare("logs", amqp.Topic, true, false, false, false, nil)
	if err != nil {
		log.Fatalf("exchange.declare: %v", err)
	}

	// Prepare this message to be persistent.  Your publishing requirements may
	// be different.
	msg := amqp.Publishing{
		DeliveryMode: amqp.Persistent,
		Timestamp:    time.Now(),
		ContentType:  "text/plain",
		Body:         []byte("Go Go AMQP!"),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// This is not a mandatory delivery, so it will be dropped if there are no
	// queues bound to the logs exchange.
	err = c.PublishWithContext(ctx, "logs", "info", false, false, msg)
	if err != nil {
		// Since publish is asynchronous this can happen if the network connection
		// is reset or if the server has run out of resources.
		log.Fatalf("basic.publish: %v", err)
	}
}

func publishAllTheThings(conn *amqp.Connection) {
	// ... snarf snarf, barf barf
}

func ExampleConnection_NotifyBlocked() {
	// Simply logs when the server throttles the TCP connection for publishers

	// Test this by tuning your server to have a low memory watermark:
	// rabbitmqctl set_vm_memory_high_watermark 0.00000001

	conn, err := amqp.Dial("amqp://guest:guest@localhost:5672/")
	if err != nil {
		log.Fatalf("connection.open: %s", err)
	}
	defer conn.Close()

	blockings := conn.NotifyBlocked(make(chan amqp.Blocking))
	go func() {
		for b := range blockings {
			if b.Active {
				log.Printf("TCP blocked: %q", b.Reason)
			} else {
				log.Printf("TCP unblocked")
			}
		}
	}()

	// Your application domain channel setup publishings
	publishAllTheThings(conn)
}

func ExampleTable_SetClientConnectionName() {
	// Sets the well-known connection_name property in amqp.Config. The connection
	// name will be visible in RabbitMQ Management UI.
	config := amqp.Config{Properties: amqp.NewConnectionProperties()}
	config.Properties.SetClientConnectionName("my-client-app")
	conn, err := amqp.DialConfig("amqp://guest:guest@localhost:5672/", config)
	if err != nil {
		log.Fatalf("connection.open: %s", err)
	}
	defer conn.Close()
}

func ExampleConnection_UpdateSecret() {
	// In order to authenticate into RabbitMQ, the application must acquire a JWT token.
	// This may be different, depending on the library used to communicate with the OAuth2
	// server. This examples assumes that it's possible to obtain tokens using username+password.
	//
	// The authentication is successful if RabbitMQ can validate the JWT with the OAuth2 server.
	// The permissions are determined from the scopes. Check the OAuth2 plugin readme for more details:
	// https://github.com/rabbitmq/rabbitmq-server/tree/main/deps/rabbitmq_auth_backend_oauth2#scope-to-permission-translation
	//
	// Once the app has a JWT token, this can be used as credentials in the URI used in Connection.Dial()
	//
	// The app should have a long-running task that checks the validity of the JWT token, and renew it before
	// the refresher time expires. Once a new JWT token is obtained, it shall be used in Connection.UpdateSecret().

	token, _ := getJWToken("username", "password")

	uri := fmt.Sprintf("amqp://%s:%s@localhost:5672", "client_id", token)
	c, _ := amqp.Dial(uri)

	defer c.Close()

	// It also calls Connection.UpdateSecret()
	tokenRefresherTask := func(conn *amqp.Connection, token string) {
		// if token is expired
		// then
		renewedToken, _ := refreshJWToken(token)
		_ = conn.UpdateSecret(renewedToken, "Token refreshed!")
	}

	go tokenRefresherTask(c, "my-JWT-token")

	ch, _ := c.Channel()
	defer ch.Close()

	_, _ = ch.QueueDeclare(
		"test-amqp",
		false,
		false,
		false,
		false,
		amqp.Table{},
	)
	_ = ch.PublishWithContext(
		context.Background(),
		"",
		"test-amqp",
		false,
		false,
		amqp.Publishing{
			Headers:         amqp.Table{},
			ContentType:     "text/plain",
			ContentEncoding: "",
			DeliveryMode:    amqp.Persistent,
			Body:            []byte("message"),
		},
	)
}

func getJWToken(username, password string) (string, error) {
	// do OAuth2 things
	return "a-token", nil
}

func refreshJWToken(token string) (string, error) {
	// do OAuth2 things to refresh tokens
	return "so fresh!", nil
}

func ExampleChannel_QueueDeclare_quorum() {
	conn, _ := amqp.Dial("amqp://localhost")
	ch, _ := conn.Channel()
	args := amqp.Table{ // queue args
		amqp.QueueTypeArg: amqp.QueueTypeQuorum,
	}
	q, _ := ch.QueueDeclare(
		"my-quorum-queue", // queue name
		true,              // durable
		false,             // auto-delete
		false,             // exclusive
		false,             // noWait
		args,
	)
	log.Printf("Declared queue: %s with arguments: %v", q.Name, args)
}

func ExampleChannel_QueueDeclare_stream() {
	conn, _ := amqp.Dial("amqp://localhost")
	ch, _ := conn.Channel()
	q, _ := ch.QueueDeclare(
		"my-stream-queue", // queue name
		true,              // durable
		false,             // auto-delete
		false,             // exclusive
		false,             // noWait
		amqp.Table{ // queue args
			amqp.QueueTypeArg:                 amqp.QueueTypeStream,
			amqp.StreamMaxLenBytesArg:         int64(5_000_000_000), // 5 Gb
			amqp.StreamMaxSegmentSizeBytesArg: 500_000_000,          // 500 Mb
			amqp.StreamMaxAgeArg:              "3D",                 // 3 days
		},
	)
	log.Printf("Declared queue: %s", q.Name)
}

func ExampleChannel_QueueDeclare_classicQueueV2() {
	conn, _ := amqp.Dial("amqp://localhost")
	ch, _ := conn.Channel()
	q, _ := ch.QueueDeclare(
		"my-classic-queue-v2", // queue name
		true,                  // durable
		false,                 // auto-delete
		false,                 // exclusive
		false,                 // noWait
		amqp.Table{
			amqp.QueueTypeArg:    amqp.QueueTypeClassic,
			amqp.QueueVersionArg: 2,
		},
	)
	log.Printf("Declared Classic Queue v2: %s", q.Name)
}

func ExampleChannel_QueueDeclare_consumerTimeout() {
	conn, _ := amqp.Dial("amqp://localhost")
	ch, _ := conn.Channel()
	// this works only with RabbitMQ 3.12+
	q, _ := ch.QueueDeclare(
		"my-classic-queue-v2", // queue name
		true,                  // durable
		false,                 // auto-delete
		false,                 // exclusive
		false,                 // noWait
		amqp.Table{
			amqp.QueueTypeArg:       amqp.QueueTypeQuorum, // also works with classic queues
			amqp.ConsumerTimeoutArg: 600_000,              // 10 minute consumer timeout
		},
	)
	log.Printf("Declared Classic Queue v2: %s", q.Name)
}
