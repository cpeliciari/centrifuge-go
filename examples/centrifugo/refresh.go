// Demonstrate how to resque from credentials expiration
// (when connection_lifetime set in Centrifugo).
package main

import (
	"fmt"
	"log"

	"github.com/centrifugal/centrifuge-go"
)

// In production you need to receive credentials from application backend.
func credentials() centrifuge.Credentials {
	// Never show secret to client of your application. Keep it on your application backend only.
	secret := "secret"
	// Application user ID.
	user := "42"
	// Exp as string.
	exp := centrifuge.Exp(60)
	// Empty info.
	info := ""
	// Generate sign so Centrifugo server can trust connection parameters received from client.
	sign := centrifuge.GenerateClientSign(secret, user, exp, info)

	return centrifuge.Credentials{
		User: user,
		Exp:  exp,
		Info: info,
		Sign: sign,
	}
}

type eventHandler struct{}

func (h *eventHandler) OnConnect(c *centrifuge.Client, e centrifuge.ConnectEvent) {
	log.Println("Connected")
}

func (h *eventHandler) OnDisconnect(c *centrifuge.Client, e centrifuge.DisconnectEvent) {
	log.Println("Disconnected")
}

func (h *eventHandler) OnRefresh(c *centrifuge.Client) (centrifuge.Credentials, error) {
	log.Println("Refresh")
	return credentials(), nil
}

type subEventHandler struct{}

func (h *subEventHandler) OnPublish(sub *centrifuge.Subscription, e centrifuge.PublishEvent) {
	log.Println(fmt.Sprintf("New message received in channel %s: %s", sub.Channel(), string(e.Data)))
}

func newConnection() *centrifuge.Client {
	creds := credentials()
	wsURL := "ws://localhost:8000/connection/websocket"

	handler := &eventHandler{}

	events := centrifuge.NewEventHub()
	events.OnDisconnect(handler)
	events.OnRefresh(handler)
	events.OnConnect(handler)

	c := centrifuge.New(wsURL, events, centrifuge.DefaultConfig())
	c.SetCredentials(&creds)

	err := c.Connect()
	if err != nil {
		log.Fatalln(err)
	}

	subEvents := centrifuge.NewSubscriptionEventHub()
	subEvents.OnPublish(&subEventHandler{})

	_, err = c.Subscribe("public:chat", subEvents)
	if err != nil {
		log.Fatalln(err)
	}

	return c
}

func main() {
	log.Println("Start program")
	newConnection()
	select {}
}
