package main

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
	mqttserver "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/hooks/auth"
	"github.com/mochi-mqtt/server/v2/listeners"
)

func startTestBroker(t *testing.T) string {
	t.Helper()
	srv := mqttserver.New(nil)
	_ = srv.AddHook(new(auth.AllowHook), nil)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	if err := srv.AddListener(listeners.NewTCP(listeners.Config{ID: "t", Address: "127.0.0.1:" + strconv.Itoa(port)})); err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	t.Cleanup(func() { srv.Close() })
	time.Sleep(50 * time.Millisecond)
	return "tcp://127.0.0.1:" + strconv.Itoa(port)
}

func TestMQTTObserveCollects(t *testing.T) {
	url := startTestBroker(t)

	go func() {
		opts := paho.NewClientOptions().AddBroker(url).SetClientID("pub")
		c := paho.NewClient(opts)
		tok := c.Connect()
		tok.WaitTimeout(2 * time.Second)
		time.Sleep(100 * time.Millisecond)
		c.Publish("test/topic", 0, false, "hello").WaitTimeout(time.Second)
	}()

	tool := NewMQTTObserveTool()
	out, err := tool.Handle(context.Background(), map[string]any{
		"broker":     url,
		"topic":      "test/#",
		"duration_s": float64(1),
	})
	if err != nil {
		t.Fatal(err)
	}
	msgs := out.(map[string]any)["messages"].([]map[string]any)
	if len(msgs) == 0 {
		t.Fatal("expected at least one message")
	}
	if msgs[0]["topic"] != "test/topic" || msgs[0]["payload"] != "hello" {
		t.Fatalf("unexpected message: %v", msgs[0])
	}
}
