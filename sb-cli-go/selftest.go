package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/gorilla/websocket"
)

// selfTestSubscribeTimeout bounds how long the self-test waits for its own
// message to arrive over the WebSocket subscription.
const selfTestSubscribeTimeout = 15 * time.Second

// selfTestStep is the result of a single self-test step.
type selfTestStep struct {
	Name      string `json:"name"`
	Pass      bool   `json:"pass"`
	LatencyMs int64  `json:"latency_ms"`
	Detail    string `json:"detail,omitempty"`
}

// selfTestReport aggregates the results of all self-test steps.
type selfTestReport struct {
	Queue string         `json:"queue"`
	OK    bool           `json:"ok"`
	Steps []selfTestStep `json:"steps"`
}

// randomQueueName returns a throwaway queue name for the self-test.
func randomQueueName() string {
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return "_sbtest-fallback"
	}
	return "_sbtest-" + hex.EncodeToString(buf)
}

// dialWS opens a WebSocket connection to a queue and sends the auth token.
func dialWS(client *ShadowClient, queue string) (*websocket.Conn, error) {
	wsURL, err := client.makeWsUrl(queue)
	if err != nil {
		return nil, err
	}
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("WS dial error: %w", err)
	}
	if err := conn.WriteJSON(WsMessage{Type: "token", Token: client.config.AuthToken}); err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to send auth token: %w", err)
	}
	return conn, nil
}

// awaitMessage reads WS messages until one whose payload contains wantBody
// arrives or the timeout elapses. It returns the message payload.
func awaitMessage(conn *websocket.Conn, wantBody string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for {
		conn.SetReadDeadline(deadline)
		var msg WsMessage
		if err := conn.ReadJSON(&msg); err != nil {
			return "", fmt.Errorf("timed out waiting for message: %w", err)
		}
		if msg.Type != "message" {
			continue
		}
		if env, isEnvelope := decodeEnvelope(msg.Message); isEnvelope && env.Body == wantBody {
			return msg.Message, nil
		}
		if msg.Message == wantBody {
			return msg.Message, nil
		}
	}
}

// runSelfTest executes the end-to-end broker self-test against queue
// (a random throwaway queue is used when queue is empty) and returns a
// per-step report. Steps: ping, keygen, signed publish, WS subscribe with
// signature verification, pull.
func runSelfTest(client *ShadowClient, queue string) selfTestReport {
	if queue == "" {
		queue = randomQueueName()
	}
	report := selfTestReport{Queue: queue}
	step := func(name string, start time.Time, pass bool, detail string) {
		report.Steps = append(report.Steps, selfTestStep{
			Name:      name,
			Pass:      pass,
			LatencyMs: time.Since(start).Milliseconds(),
			Detail:    detail,
		})
	}

	// (a) ping
	start := time.Now()
	pingOK, _, err := client.Ping()
	if err != nil {
		step("ping", start, false, err.Error())
	} else {
		step("ping", start, pingOK, "")
	}

	// (b) keygen (in memory only)
	start = time.Now()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		step("keygen", start, false, err.Error())
	} else {
		step("keygen", start, true, "")
	}

	// Prepare a unique signed message body.
	body := "sb-selftest " + randomQueueName()

	// The WS subscription is opened before publishing: the broker broadcasts
	// only newly-published messages, so the subscriber must be attached first.
	// Failures here are reported on the subscribe step below.
	var wsConn *websocket.Conn
	var wsErr error
	if priv != nil {
		wsConn, wsErr = dialWS(client, queue)
		if wsConn != nil {
			defer wsConn.Close()
		}
	}

	// (c) publish a signed envelope
	start = time.Now()
	publishOK := false
	if priv == nil {
		step("publish", start, false, "keygen failed, nothing to sign with")
	} else {
		signed, signErr := signEnvelope(body, "sb-selftest", priv)
		if signErr != nil {
			step("publish", start, false, signErr.Error())
		} else if _, pubErr := client.Publish(queue, signed); pubErr != nil {
			step("publish", start, false, pubErr.Error())
		} else {
			publishOK = true
			step("publish", start, true, "")
		}
	}

	// (d) receive the message over WS subscribe and verify the signature
	start = time.Now()
	switch {
	case !publishOK:
		step("subscribe-verify", start, false, "publish failed, nothing to receive")
	case wsErr != nil:
		step("subscribe-verify", start, false, wsErr.Error())
	default:
		raw, waitErr := awaitMessage(wsConn, body, selfTestSubscribeTimeout)
		if waitErr != nil {
			step("subscribe-verify", start, false, waitErr.Error())
		} else if _, isEnvelope, valid := parseEnvelope(raw, pub); !isEnvelope {
			step("subscribe-verify", start, false, "received payload is not a signed envelope")
		} else if !valid {
			step("subscribe-verify", start, false, "signature verification failed")
		} else {
			step("subscribe-verify", start, true, "")
		}
	}

	// (e) pull and check the message is present
	start = time.Now()
	if !publishOK {
		step("pull", start, false, "publish failed, nothing to pull")
	} else if pulled, pullErr := client.Pull(queue, 0, 100); pullErr != nil {
		step("pull", start, false, pullErr.Error())
	} else {
		found := false
		for _, m := range pulled.Messages {
			if env, isEnvelope := decodeEnvelope(m.Message); isEnvelope && env.Body == body {
				found = true
				break
			}
		}
		if found {
			step("pull", start, true, "")
		} else {
			step("pull", start, false, "message not found in pull response")
		}
	}

	report.OK = true
	for _, s := range report.Steps {
		if !s.Pass {
			report.OK = false
			break
		}
	}
	return report
}

// printSelfTestReport renders the report either as one JSON object (--json)
// or as a human-readable PASS/FAIL listing.
func printSelfTestReport(report selfTestReport, jsonOut bool) {
	if jsonOut {
		rawJSON, err := json.Marshal(report)
		if err != nil {
			fmt.Printf("{\"ok\":false,\"error\":%q}\n", err.Error())
			return
		}
		fmt.Println(string(rawJSON))
		return
	}
	fmt.Printf("Self-test against queue %q:\n", report.Queue)
	for _, s := range report.Steps {
		status := "PASS"
		if !s.Pass {
			status = "FAIL"
		}
		line := fmt.Sprintf("  [%s] %-16s (%d ms)", status, s.Name, s.LatencyMs)
		if s.Detail != "" {
			line += " - " + s.Detail
		}
		fmt.Println(line)
	}
	if report.OK {
		fmt.Println("SELF-TEST OK")
	} else {
		fmt.Println("SELF-TEST FAILED")
	}
}
