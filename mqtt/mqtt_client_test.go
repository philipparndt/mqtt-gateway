package mqtt

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/eclipse/paho.mqtt.golang/packets"
	"github.com/philipparndt/mqtt-gateway/config"
)

// fakeBroker is a minimal MQTT 3.1.1 broker: it acknowledges everything and
// records the publishes it receives.
type fakeBroker struct {
	ln net.Listener

	mu        sync.Mutex
	conns     []net.Conn
	published []string
}

func startBroker(t *testing.T, addr string) *fakeBroker {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	b := &fakeBroker{ln: ln}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			b.mu.Lock()
			b.conns = append(b.conns, conn)
			b.mu.Unlock()
			go b.serve(conn)
		}
	}()
	t.Cleanup(b.stop)
	return b
}

func (b *fakeBroker) addr() string { return b.ln.Addr().String() }

func (b *fakeBroker) serve(conn net.Conn) {
	defer conn.Close()
	for {
		cp, err := packets.ReadPacket(conn)
		if err != nil {
			return
		}
		var reply packets.ControlPacket
		switch p := cp.(type) {
		case *packets.ConnectPacket:
			reply = packets.NewControlPacket(packets.Connack)
		case *packets.PublishPacket:
			b.mu.Lock()
			b.published = append(b.published, p.TopicName+"="+string(p.Payload))
			b.mu.Unlock()
			switch p.Qos {
			case 1:
				ack := packets.NewControlPacket(packets.Puback).(*packets.PubackPacket)
				ack.MessageID = p.MessageID
				reply = ack
			case 2:
				rec := packets.NewControlPacket(packets.Pubrec).(*packets.PubrecPacket)
				rec.MessageID = p.MessageID
				reply = rec
			}
		case *packets.PubrelPacket:
			comp := packets.NewControlPacket(packets.Pubcomp).(*packets.PubcompPacket)
			comp.MessageID = p.MessageID
			reply = comp
		case *packets.SubscribePacket:
			ack := packets.NewControlPacket(packets.Suback).(*packets.SubackPacket)
			ack.MessageID = p.MessageID
			ack.ReturnCodes = make([]byte, len(p.Topics))
			reply = ack
		case *packets.PingreqPacket:
			reply = packets.NewControlPacket(packets.Pingresp)
		case *packets.DisconnectPacket:
			return
		}
		if reply != nil {
			if err := reply.Write(conn); err != nil {
				return
			}
		}
	}
}

// stop closes the listener and all client connections, like a broker going
// down.
func (b *fakeBroker) stop() {
	b.ln.Close()
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, c := range b.conns {
		c.Close()
	}
	b.conns = nil
}

func (b *fakeBroker) has(msg string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, m := range b.published {
		if m == msg {
			return true
		}
	}
	return false
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// setup applies fast test settings and restores the package state afterwards.
func setup(t *testing.T) {
	t.Helper()
	oldRetry, oldMax, oldDrop, oldTimeout := RetryInterval, MaxReconnectInterval, DropWhenDisconnected, PublishTimeout
	RetryInterval = 50 * time.Millisecond
	MaxReconnectInterval = 100 * time.Millisecond
	t.Cleanup(func() {
		Stop()
		SetOnReconnect(nil)
		RetryInterval, MaxReconnectInterval, DropWhenDisconnected, PublishTimeout = oldRetry, oldMax, oldDrop, oldTimeout
	})
}

// freeAddr returns a local address nothing listens on.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func TestStartContext_WaitsForBroker(t *testing.T) {
	setup(t)
	addr := freeAddr(t)

	var b *fakeBroker
	var mu sync.Mutex
	go func() {
		time.Sleep(200 * time.Millisecond)
		mu.Lock()
		b = startBroker(t, addr)
		mu.Unlock()
	}()

	start := time.Now()
	if err := StartContext(context.Background(), config.MQTTConfig{URL: "tcp://" + addr, Topic: "test"}, "t"); err != nil {
		t.Fatalf("StartContext: %v", err)
	}
	if time.Since(start) < 200*time.Millisecond {
		t.Error("StartContext returned before the broker was up")
	}
	mu.Lock()
	defer mu.Unlock()
	waitFor(t, "online status", func() bool { return b.has("test/bridge/state=online") })
}

func TestStartContext_Cancelled(t *testing.T) {
	setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	err := StartContext(ctx, config.MQTTConfig{URL: "tcp://" + freeAddr(t), Topic: "test"}, "t")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
}

func TestReconnect_DropsWhileDownAndCallsOnReconnect(t *testing.T) {
	setup(t)
	DropWhenDisconnected = true
	b := startBroker(t, "127.0.0.1:0")
	addr := b.addr()

	reconnected := make(chan struct{}, 1)
	SetOnReconnect(func() {
		PublishAbsolute("test/resync", "yes", false)
		reconnected <- struct{}{}
	})

	if err := StartContext(context.Background(), config.MQTTConfig{URL: "tcp://" + addr, Topic: "test"}, "t"); err != nil {
		t.Fatalf("StartContext: %v", err)
	}
	PublishAbsolute("test/a", "1", false)
	waitFor(t, "first publish", func() bool { return b.has("test/a=1") })

	b.stop()
	waitFor(t, "connection loss", func() bool { return !client.IsConnectionOpen() })

	done := make(chan struct{})
	go func() {
		PublishAbsolute("test/a", "dropped", false)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("PublishAbsolute blocked while disconnected")
	}

	b2 := startBroker(t, addr)
	select {
	case <-reconnected:
	case <-time.After(10 * time.Second):
		t.Fatal("OnReconnect was not called")
	}
	waitFor(t, "online status after reconnect", func() bool { return b2.has("test/bridge/state=online") })
	waitFor(t, "publish from OnReconnect", func() bool { return b2.has("test/resync=yes") })
	if b2.has("test/a=dropped") {
		t.Error("message published while disconnected was delivered")
	}
}
