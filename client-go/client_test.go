package shadowbroker

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

// TestRoundTrip needs a live broker: SHADOW_BROKER_CONFIG in the env.
func TestRoundTrip(t *testing.T) {
	if os.Getenv("SHADOW_BROKER_CONFIG") == "" {
		t.Skip("SHADOW_BROKER_CONFIG not set")
	}
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := c.Health(ctx); err != nil {
		t.Fatal(err)
	}
	queue := fmt.Sprintf("client-go.test.%d.control", time.Now().UnixNano())

	got := make(chan Message, 1)
	connected := make(chan struct{})
	var once sync.Once
	go c.Subscribe(ctx, queue, func(m Message) { got <- m },
		OnConnect(func() { once.Do(func() { close(connected) }) }),
		OnDisconnect(func(err error) { t.Logf("ws disconnected: %v", err) }))
	select {
	case <-connected:
	case <-ctx.Done():
		t.Fatal("subscribe did not connect")
	}
	// The broker sends no handshake ack: OnConnect fires when the token has been
	// written, the server registers the subscriber a moment later. Real code
	// Pulls from the last processed id after OnConnect; a test just waits.
	time.Sleep(300 * time.Millisecond)

	id, err := c.Publish(ctx, queue, `{"hello":"go"}`, Lane("control"))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-got:
		if m.ID != id || m.Message != `{"hello":"go"}` {
			t.Fatalf("stream mismatch: %+v (want id %d)", m, id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no message on websocket within 5 s")
	}
	msgs, last, err := c.Pull(ctx, queue, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].ID != id || last != id {
		t.Fatalf("pull mismatch: %+v last=%d", msgs, last)
	}
}
