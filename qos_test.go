package main

import (
	"context"
	"encoding/json"
	"github.com/gorilla/websocket"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPublishWakesPollerAndDeliversWithoutOneSecondTick(t *testing.T) {
	store = newTestStore(t, "file:"+filepath.Join(t.TempDir(), "broker.db"))
	globalMaxID = 0
	gConf = BrokerConfig{SecretKey: strings.Repeat("x", 32), AuthToken: "test-only-token"}
	clients = make(map[string]map[*wsClient]bool)
	wakePoller = make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { defer close(stopped); runPoller(ctx) }()
	defer func() { cancel(); <-stopped }()
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", wsHandler)
	mux.HandleFunc("/publish", publishHandler)
	server := httptest.NewServer(mux)
	defer server.Close()
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/ws?queue=barber.control", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	ws.WriteJSON(map[string]string{"token": gConf.AuthToken})
	ws.WriteJSON(map[string]string{"type": "ping"})
	ws.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err = ws.ReadMessage(); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("POST", server.URL+"/publish", strings.NewReader(`{"queue":"barber.control","message":"stop"}`))
	req.Header.Set("X-Auth", gConf.AuthToken)
	start := time.Now()
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal(response.StatusCode)
	}
	ws.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	_, payload, err := ws.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]interface{}
	if err = json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["message"] != "stop" {
		t.Fatalf("unexpected message: %s", payload)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("publish waited behind poll interval")
	}
}

func TestFullContentSubscriberQueueDoesNotBlockControlSubscriber(t *testing.T) {
	slow := &wsClient{out: make(chan []byte, 1), done: make(chan struct{})}
	fast := &wsClient{out: make(chan []byte, 1), done: make(chan struct{})}
	if err := slow.writeMessage([]byte("large content")); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := slow.writeMessage([]byte("more content")); err == nil {
		t.Fatal("full queue must reject")
	}
	if err := fast.writeMessage([]byte("stop")); err != nil {
		t.Fatal(err)
	}
	select {
	case data := <-fast.out:
		if string(data) != "stop" {
			t.Fatal("wrong control")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("control stalled")
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("blocked by slow content")
	}
}
