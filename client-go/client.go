// Package shadowbroker is a small Go client for Shadow Broker
// (https://github.com/c3z/shadow-broker-server): publish, pull, subscribe and
// health over the three-endpoint wire protocol (docs/protocol.md). One
// dependency: gorilla/websocket.
//
//	cfg, _ := shadowbroker.ConfigFromEnv()          // SHADOW_BROKER_CONFIG
//	c, _ := shadowbroker.New(cfg)
//	id, _ := c.Publish(ctx, "jobs.control", `{"cmd":"stop"}`, shadowbroker.Lane("control"))
//	msgs, last, _ := c.Pull(ctx, "jobs.control", 0, 100)
//	_ = c.Subscribe(ctx, "jobs.control", func(m shadowbroker.Message) { ... })
//
// Messages are opaque strings to the broker; put JSON (or a signed envelope,
// see docs/security.md) in them. The broker never replays history to a
// WebSocket subscriber: keep the last id you processed and Pull after a
// reconnect if you need gapless delivery.
package shadowbroker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/gorilla/websocket"
)

// Config is the client-side part of SHADOW_BROKER_CONFIG.
type Config struct {
	URL       string `json:"url"`
	AuthToken string `json:"auth_token"`
}

// ConfigFromEnv decodes SHADOW_BROKER_CONFIG (base64 JSON). Server-only
// fields (secret_key, db_url) are ignored.
func ConfigFromEnv() (Config, error) {
	raw := os.Getenv("SHADOW_BROKER_CONFIG")
	if raw == "" {
		return Config{}, errors.New("SHADOW_BROKER_CONFIG is not set")
	}
	b, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return Config{}, fmt.Errorf("SHADOW_BROKER_CONFIG is not base64: %w", err)
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return Config{}, fmt.Errorf("SHADOW_BROKER_CONFIG is not JSON: %w", err)
	}
	if c.URL == "" || c.AuthToken == "" {
		return Config{}, errors.New("SHADOW_BROKER_CONFIG needs url and auth_token")
	}
	return c, nil
}

// Client talks to one broker. It is safe for concurrent use.
type Client struct {
	cfg  Config
	base *url.URL
	HTTP *http.Client // replace to tune timeouts; default 15 s
}

// New validates the config and returns a client.
func New(cfg Config) (*Client, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("invalid broker url %q", cfg.URL)
	}
	if cfg.AuthToken == "" {
		return nil, errors.New("auth token is empty")
	}
	return &Client{cfg: cfg, base: u, HTTP: &http.Client{Timeout: 15 * time.Second}}, nil
}

// Message is one stored or streamed message.
type Message struct {
	ID        int64  `json:"id"`
	Message   string `json:"message"`
	CreatedAt string `json:"created_at,omitempty"` // RFC 3339, pull only
}

// PublishOption tweaks one publish request.
type PublishOption func(*http.Request)

// Lane sets X-Broker-Lane. "control" gives the request the broker's separate
// control rate-limit budget (QoS). Dispatch priority itself comes from the
// queue name ending in ".control".
func Lane(lane string) PublishOption {
	return func(r *http.Request) { r.Header.Set("X-Broker-Lane", lane) }
}

func (c *Client) endpoint(path string, q url.Values) string {
	u := *c.base
	u.Path = path
	u.RawQuery = q.Encode()
	return u.String()
}

// Publish stores message in queue and returns its id.
func (c *Client) Publish(ctx context.Context, queue, message string, opts ...PublishOption) (int64, error) {
	if queue == "" || message == "" {
		return 0, errors.New("queue and message must be non-empty")
	}
	body, _ := json.Marshal(map[string]string{"queue": queue, "message": message})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint("/publish", nil), bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Auth", c.cfg.AuthToken)
	for _, o := range opts {
		o(req)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return 0, fmt.Errorf("publish: %s: %s", resp.Status, bytes.TrimSpace(b))
	}
	var out struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.ID == 0 {
		return 0, errors.New("publish: malformed response")
	}
	return out.ID, nil
}

// Pull returns stored messages with id > since, oldest first, and the id to
// pass as the next since. limit is capped server-side.
func (c *Client) Pull(ctx context.Context, queue string, since int64, limit int) ([]Message, int64, error) {
	if queue == "" {
		return nil, 0, errors.New("queue must be non-empty")
	}
	q := url.Values{"queue": {queue}, "since": {strconv.FormatInt(since, 10)}}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint("/pull", q), nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("X-Auth", c.cfg.AuthToken)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, 0, fmt.Errorf("pull: %s: %s", resp.Status, bytes.TrimSpace(b))
	}
	var out struct {
		Messages []Message `json:"messages"`
		LastID   int64     `json:"last_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, 0, fmt.Errorf("pull: malformed response: %w", err)
	}
	if out.LastID == 0 {
		out.LastID = since
	}
	return out.Messages, out.LastID, nil
}

// Health is the unauthenticated liveness probe (GET /health). nil means the
// process is up and its store answers.
func (c *Client) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint("/health", nil), nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health: %s", resp.Status)
	}
	return nil
}

// SubscribeOption tweaks Subscribe.
type SubscribeOption func(*subscribeOptions)

type subscribeOptions struct {
	onConnect    func()
	onDisconnect func(error)
	maxBackoff   time.Duration
}

// OnConnect runs after every successful handshake (first connect and each
// reconnect). Use it to Pull the gap since the last id you processed.
func OnConnect(f func()) SubscribeOption { return func(o *subscribeOptions) { o.onConnect = f } }

// OnDisconnect runs with the error that ended a connection before the next
// reconnect attempt.
func OnDisconnect(f func(error)) SubscribeOption {
	return func(o *subscribeOptions) { o.onDisconnect = f }
}

// MaxBackoff caps the reconnect delay (default 30 s; it starts at 1 s and doubles).
func MaxBackoff(d time.Duration) SubscribeOption {
	return func(o *subscribeOptions) { o.maxBackoff = d }
}

const wsReadTimeout = 60 * time.Second // the broker pings every 15 s

// Subscribe streams new messages for queue to fn until ctx is done,
// reconnecting with exponential backoff. fn runs on the reader goroutine;
// return quickly or hand off. The only non-nil return is ctx.Err().
func (c *Client) Subscribe(ctx context.Context, queue string, fn func(Message), opts ...SubscribeOption) error {
	if queue == "" {
		return errors.New("queue must be non-empty")
	}
	o := subscribeOptions{maxBackoff: 30 * time.Second}
	for _, opt := range opts {
		opt(&o)
	}
	backoff := time.Second
	for {
		err := c.subscribeOnce(ctx, queue, fn, o)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if o.onDisconnect != nil {
			o.onDisconnect(err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < o.maxBackoff {
			backoff *= 2
		}
	}
}

func (c *Client) subscribeOnce(ctx context.Context, queue string, fn func(Message), o subscribeOptions) error {
	u := *c.base
	u.Scheme = map[string]string{"http": "ws", "https": "wss"}[u.Scheme]
	u.Path = "/ws"
	u.RawQuery = url.Values{"queue": {queue}}.Encode()

	conn, _, err := websocket.DefaultDialer.DialContext(ctx, u.String(), nil)
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetReadLimit(32 << 20)
	if err := conn.WriteJSON(map[string]string{"token": c.cfg.AuthToken}); err != nil {
		return err
	}
	conn.SetReadDeadline(time.Now().Add(wsReadTimeout))
	conn.SetPingHandler(func(data string) error {
		conn.SetReadDeadline(time.Now().Add(wsReadTimeout))
		return conn.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(5*time.Second))
	})

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
		}
	}()
	if o.onConnect != nil {
		o.onConnect()
	}
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		conn.SetReadDeadline(time.Now().Add(wsReadTimeout))
		var ev struct {
			Type    string `json:"type"`
			ID      int64  `json:"id"`
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &ev) != nil {
			continue
		}
		if ev.Type == "message" {
			fn(Message{ID: ev.ID, Message: ev.Message})
		}
	}
}
