package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	mrand "math/rand"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

// Config holds the configuration loaded from the environment.
type Config struct {
	URL       string `json:"url"`
	AuthToken string `json:"auth_token"`
}

// ShadowClient interacts with the ShadowBroker server.
type ShadowClient struct {
	config              Config
	httpClient          *http.Client
	wsReconnectMutex    sync.Mutex
	wsConn              *websocket.Conn
	wsIsConnected       bool
	wsReconnectAttempts int
	wsMaxReconnects     int
	wsReconnectDelay    time.Duration
	wsStopChan          chan struct{} // Channel to signal stopping reconnection attempts

	// onMessage, when set, receives every incoming WS message (id + raw
	// payload) instead of the default stdout printing in wsReader.
	onMessage func(id int64, raw string)
	// onState, when set, is notified whenever the WS connection state
	// changes (connected after a successful dial, disconnected otherwise).
	onState func(connected bool)
}

// PublishResponse is the expected JSON response from the /publish endpoint.
type PublishResponse struct {
	ID int64 `json:"id"`
}

// Envelope is a signed message payload. Fields are declared in alphabetical
// order so json.Marshal produces the canonical compact form.
type Envelope struct {
	Body string `json:"body"`
	From string `json:"from"`
	Sig  string `json:"sig"` // base64-encoded Ed25519 signature
	Ts   int64  `json:"ts"`
	V    int    `json:"v"`
}

// canonicalPayload returns the exact byte string that is signed:
// "v1\n" + from + "\n" + <ts as decimal> + "\n" + body (UTF-8, no trailing newline).
func canonicalPayload(from string, ts int64, body string) []byte {
	return []byte("v1\n" + from + "\n" + strconv.FormatInt(ts, 10) + "\n" + body)
}

// signEnvelope wraps body into a signed Envelope and returns its compact JSON.
func signEnvelope(body, from string, priv ed25519.PrivateKey) (string, error) {
	if from == "" {
		return "", errors.New("sender name (--from) must be a non-empty string")
	}
	if len(priv) != ed25519.PrivateKeySize {
		return "", fmt.Errorf("invalid private key size: %d", len(priv))
	}
	env := Envelope{Body: body, From: from, Ts: time.Now().Unix(), V: 1}
	sig := ed25519.Sign(priv, canonicalPayload(env.From, env.Ts, env.Body))
	env.Sig = base64.StdEncoding.EncodeToString(sig)
	raw, err := json.Marshal(env)
	if err != nil {
		return "", fmt.Errorf("failed to marshal envelope: %w", err)
	}
	return string(raw), nil
}

// decodeEnvelope tries to decode raw as an Envelope without verifying the
// signature. It reports whether raw looked like an envelope at all.
func decodeEnvelope(raw string) (env Envelope, isEnvelope bool) {
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		return env, false
	}
	if env.V != 1 || env.From == "" || env.Sig == "" {
		return env, false
	}
	return env, true
}

// parseEnvelope tries to decode raw as an Envelope. It reports whether raw
// looked like an envelope at all (isEnvelope), and whether the signature is
// valid against pub.
func parseEnvelope(raw string, pub ed25519.PublicKey) (env Envelope, isEnvelope, valid bool) {
	env, isEnvelope = decodeEnvelope(raw)
	if !isEnvelope {
		return env, false, false
	}
	sig, err := base64.StdEncoding.DecodeString(env.Sig)
	if err != nil {
		return env, true, false
	}
	valid = ed25519.Verify(pub, canonicalPayload(env.From, env.Ts, env.Body), sig)
	return env, true, valid
}

// loadKey reads an Ed25519 key from a file (containing base64) or, if the
// argument is not an existing file, treats it as a base64 string directly.
func loadKey(ref string, expectedSize int, kind string) ([]byte, error) {
	data := ref
	if info, err := os.Stat(ref); err == nil && !info.IsDir() {
		content, err := os.ReadFile(ref)
		if err != nil {
			return nil, fmt.Errorf("failed to read %s key file: %w", kind, err)
		}
		data = strings.TrimSpace(string(content))
	}
	key, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return nil, fmt.Errorf("invalid base64 %s key: %w", kind, err)
	}
	if len(key) != expectedSize {
		return nil, fmt.Errorf("invalid %s key size: got %d bytes, expected %d", kind, len(key), expectedSize)
	}
	return key, nil
}

// extractFlag removes "--name value" or "--name=value" from args and returns
// its value, the remaining args, and whether it was found. The returned slice
// is freshly allocated: sharing the backing array of args would let the
// append overwrite the extracted value before it is read.
func extractFlag(args []string, name string) (value string, rest []string, found bool) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--"+name && i+1 < len(args) {
			value = args[i+1]
			rest = make([]string, 0, len(args)-2)
			rest = append(rest, args[:i]...)
			rest = append(rest, args[i+2:]...)
			return value, rest, true
		}
		if strings.HasPrefix(arg, "--"+name+"=") {
			value = strings.TrimPrefix(arg, "--"+name+"=")
			rest = make([]string, 0, len(args)-1)
			rest = append(rest, args[:i]...)
			rest = append(rest, args[i+1:]...)
			return value, rest, true
		}
	}
	return "", args, false
}

// extractBoolFlag removes a bare "--name" switch from args and reports
// whether it was present. The returned slice is freshly allocated (see
// extractFlag for why).
func extractBoolFlag(args []string, name string) (rest []string, found bool) {
	for i, arg := range args {
		if arg == "--"+name {
			rest = make([]string, 0, len(args)-1)
			rest = append(rest, args[:i]...)
			rest = append(rest, args[i+1:]...)
			return rest, true
		}
	}
	return args, false
}

// formatReceived renders an incoming message, verifying its signature when
// a public key is provided.
func formatReceived(raw string, pub ed25519.PublicKey) string {
	if pub == nil {
		return raw
	}
	env, isEnvelope, valid := parseEnvelope(raw, pub)
	if !isEnvelope {
		return raw
	}
	if valid {
		return fmt.Sprintf("[%s ✓] %s", env.From, env.Body)
	}
	return fmt.Sprintf("[INVALID SIGNATURE] (claims from=%s) %s", env.From, env.Body)
}

// jsonReceivedMessage is the machine-readable (--json) representation of a
// message received over WebSocket.
type jsonReceivedMessage struct {
	ID       int64  `json:"id"`
	Message  string `json:"message"`
	From     string `json:"from,omitempty"`
	Verified *bool  `json:"verified,omitempty"`
}

// formatReceivedJSON renders an incoming WS message as one compact JSON
// object. When the payload is a signed envelope, the body is unwrapped and
// the "from" field is set; "verified" is only present when a public key was
// configured for verification.
func formatReceivedJSON(id int64, raw string, pub ed25519.PublicKey) string {
	out := jsonReceivedMessage{ID: id, Message: raw}
	env, isEnvelope := decodeEnvelope(raw)
	if isEnvelope {
		out.Message = env.Body
		out.From = env.From
		if pub != nil {
			_, _, valid := parseEnvelope(raw, pub)
			out.Verified = &valid
		}
	}
	rawJSON, err := json.Marshal(out)
	if err != nil {
		// Message payloads are strings; marshalling can only fail on
		// programmer error, so fall back to the raw payload.
		return fmt.Sprintf(`{"id":%d,"message":%q}`, id, raw)
	}
	return string(rawJSON)
}

// PullMessage represents a single message retrieved from the /pull endpoint.
// CreatedAt is kept as a string because the broker returns it in the
// database's native datetime format ("2006-01-02 15:04:05"), not RFC3339.
type PullMessage struct {
	ID        int64  `json:"id"`
	Message   string `json:"message"`
	CreatedAt string `json:"created_at"`
}

// PullResponse is the expected JSON response from the /pull endpoint.
type PullResponse struct {
	Messages []PullMessage `json:"messages"`
	LastID   int64         `json:"last_id"`
}

// WsMessage represents a message structure used over WebSocket.
type WsMessage struct {
	Type    string `json:"type"`              // "message", "ping", "pong", "error", "token"
	ID      int64  `json:"id,omitempty"`      // Message ID for "message" type
	Message string `json:"message,omitempty"` // Content for message/error types
	Token   string `json:"token,omitempty"`   // Auth token sent after connection
}

// loadConfig reads, decodes, and parses the config from SHADOW_BROKER_CONFIG env var.
func loadConfig() (Config, error) {
	var config Config
	b64Config := os.Getenv("SHADOW_BROKER_CONFIG")
	if b64Config == "" {
		return config, errors.New("missing SHADOW_BROKER_CONFIG in the environment")
	}

	jsonBytes, err := base64.StdEncoding.DecodeString(b64Config)
	if err != nil {
		return config, fmt.Errorf("failed to decode base64 config: %w", err)
	}

	err = json.Unmarshal(jsonBytes, &config)
	if err != nil {
		return config, fmt.Errorf("invalid JSON in SHADOW_BROKER_CONFIG: %w", err)
	}

	if config.URL == "" {
		return config, errors.New("missing url in config")
	}
	if config.AuthToken == "" {
		return config, errors.New("missing auth_token in config")
	}

	// Ensure URL has a trailing slash for consistency
	if !strings.HasSuffix(config.URL, "/") {
		config.URL += "/"
	}

	return config, nil
}

// NewShadowClient creates a new client instance.
func NewShadowClient(config Config) *ShadowClient {
	return &ShadowClient{
		config:          config,
		httpClient:      &http.Client{Timeout: 10 * time.Second},
		wsMaxReconnects: 5,
		wsStopChan:      make(chan struct{}),
	}
}

// buildURL constructs a full URL string relative to the base URL in the config.
func (c *ShadowClient) buildURL(path string) (string, error) {
	base, err := url.Parse(c.config.URL)
	if err != nil {
		return "", fmt.Errorf("invalid base URL in config: %w", err)
	}
	relative, err := url.Parse(path)
	if err != nil {
		return "", fmt.Errorf("invalid path: %w", err)
	}
	return base.ResolveReference(relative).String(), nil
}

// Publish sends a message to a queue.
func (c *ShadowClient) Publish(queue, message string) (int64, error) {
	if queue == "" {
		return 0, errors.New("queue name must be a non-empty string")
	}
	if message == "" {
		return 0, errors.New("message must be a non-empty string")
	}

	publishURL, err := c.buildURL("publish")
	if err != nil {
		return 0, err
	}

	payload := map[string]string{"queue": queue, "message": message}
	jsonPayload, _ := json.Marshal(payload)

	req, err := http.NewRequest("POST", publishURL, bytes.NewBuffer(jsonPayload))
	if err != nil {
		return 0, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Auth", c.config.AuthToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("publish request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("publish failed: %s - %s", resp.Status, string(bodyBytes))
	}

	var result PublishResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, fmt.Errorf("failed to decode publish response: %w", err)
	}

	if result.ID == 0 {
		return 0, errors.New("invalid response from server: missing message ID")
	}

	log.Printf("Published to queue \"%s\" with ID: %d", queue, result.ID)
	return result.ID, nil
}

// Pull retrieves messages from a queue.
func (c *ShadowClient) Pull(queue string, since int64, limit int) (*PullResponse, error) {
	if queue == "" {
		return nil, errors.New("queue name must be a non-empty string")
	}
	if since < 0 {
		return nil, errors.New("since must be >= 0")
	}
	if limit <= 0 || limit > 1000 {
		return nil, errors.New("limit must be between 1 and 1000")
	}

	pullURL, err := c.buildURL("pull")
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("GET", pullURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	q := req.URL.Query()
	q.Add("queue", queue)
	q.Add("since", strconv.FormatInt(since, 10))
	q.Add("limit", strconv.Itoa(limit))
	req.URL.RawQuery = q.Encode()

	req.Header.Set("X-Auth", c.config.AuthToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pull request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("pull failed: %s - %s", resp.Status, string(bodyBytes))
	}

	var result PullResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode pull response: %w", err)
	}

	log.Printf("Pulled %d messages from queue \"%s\" (since=%d, limit=%d)", len(result.Messages), queue, since, limit)
	return &result, nil
}

// makeWsUrl constructs the WebSocket URL, converting http(s) to ws(s).
func (c *ShadowClient) makeWsUrl(queue string) (string, error) {
	baseURL, err := url.Parse(c.config.URL)
	if err != nil {
		return "", fmt.Errorf("invalid base URL: %w", err)
	}

	switch baseURL.Scheme {
	case "http":
		baseURL.Scheme = "ws"
	case "https":
		baseURL.Scheme = "wss"
	default:
		return "", fmt.Errorf("unsupported URL scheme: %s", baseURL.Scheme)
	}

	wsURL := baseURL.ResolveReference(&url.URL{Path: "ws"})
	q := wsURL.Query()
	q.Set("queue", queue)
	wsURL.RawQuery = q.Encode()

	return wsURL.String(), nil
}

// Subscribe connects to a queue via WebSocket and handles messages.
// If pubKey is non-nil, incoming messages that parse as signed envelopes
// are verified against it.
func (c *ShadowClient) Subscribe(queue string, pubKey ed25519.PublicKey) error {
	if queue == "" {
		return errors.New("queue name must be a non-empty string")
	}

	c.wsReconnectDelay = 1 * time.Second // Reset delay on initial call
	c.wsReconnectAttempts = 0

	stopSignal := make(chan os.Signal, 1)
	signal.Notify(stopSignal, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-stopSignal
		log.Println("Received interrupt signal, closing WebSocket...")
		close(c.wsStopChan) // Signal the connect loop to stop trying
		c.wsReconnectMutex.Lock()
		if c.wsConn != nil {
			// Send a close message
			c.wsConn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
			c.wsConn.Close()
			c.wsConn = nil // Prevent further use
		}
		c.wsReconnectMutex.Unlock()
		os.Exit(0) // Exit the program cleanly
	}()

	c.connectWebSocket(queue, pubKey) // Initial connection attempt

	// Keep main goroutine alive - connectWebSocket handles the reading loop
	select {}
}

// notifyState informs the optional onState handler about a connection state
// change. It must be called without holding wsReconnectMutex, as handlers
// (e.g. the TUI) may block briefly.
func (c *ShadowClient) notifyState(connected bool) {
	if c.onState != nil {
		c.onState(connected)
	}
}

// StartSubscribe begins the WebSocket connection/read loop in the background
// using the client's onMessage/onState handlers. Unlike Subscribe it does not
// install a signal handler and does not block; close c.wsStopChan (via
// StopSubscribe) to shut it down. Used by the TUI.
func (c *ShadowClient) StartSubscribe(queue string, pubKey ed25519.PublicKey) {
	c.wsReconnectDelay = 1 * time.Second
	c.wsReconnectAttempts = 0
	go c.connectWebSocket(queue, pubKey)
}

// StopSubscribe signals the connection loop started by StartSubscribe to
// stop and closes the current connection, if any.
func (c *ShadowClient) StopSubscribe() {
	select {
	case <-c.wsStopChan:
		// Already stopped.
	default:
		close(c.wsStopChan)
	}
	c.wsReconnectMutex.Lock()
	if c.wsConn != nil {
		c.wsConn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		c.wsConn.Close()
		c.wsConn = nil
	}
	c.wsReconnectMutex.Unlock()
}

// IsConnected reports the current WebSocket connection state.
func (c *ShadowClient) IsConnected() bool {
	c.wsReconnectMutex.Lock()
	defer c.wsReconnectMutex.Unlock()
	return c.wsIsConnected
}

func (c *ShadowClient) connectWebSocket(queue string, pubKey ed25519.PublicKey) {
	c.wsReconnectMutex.Lock()
	defer c.wsReconnectMutex.Unlock()

	// Check if stop signal was received before attempting connection/reconnection
	select {
	case <-c.wsStopChan:
		log.Println("Stopping WebSocket connection attempts.")
		return
	default:
		// Continue with connection attempt
	}

	wsUrl, err := c.makeWsUrl(queue)
	if err != nil {
		log.Printf("Error creating WebSocket URL: %v", err)
		c.scheduleReconnectLocked(queue, pubKey)
		return
	}

	log.Printf("Connecting to WebSocket: %s", wsUrl)
	conn, _, err := websocket.DefaultDialer.Dial(wsUrl, nil) // No custom headers needed here
	if err != nil {
		log.Printf("WebSocket dial error: %v", err)
		c.scheduleReconnectLocked(queue, pubKey)
		return
	}

	log.Printf("WebSocket open for queue: %s, sending auth token...", queue)
	authMsg := WsMessage{Type: "token", Token: c.config.AuthToken}
	if err := conn.WriteJSON(authMsg); err != nil {
		log.Printf("Error sending auth token: %v", err)
		conn.Close()
		c.scheduleReconnectLocked(queue, pubKey)
		return
	}

	// --- Connection successful ---
	c.wsConn = conn
	c.wsIsConnected = true
	c.wsReconnectAttempts = 0            // Reset attempts on successful connection
	c.wsReconnectDelay = 1 * time.Second // Reset delay

	// Start reader and writer goroutines
	go c.wsReader(queue, pubKey)
	go c.wsHeartbeat(queue) // Start heartbeat after successful connection
	go c.notifyState(true)
}

func (c *ShadowClient) wsReader(queue string, pubKey ed25519.PublicKey) {
	// Ensure connection is closed if reader exits
	defer func() {
		c.wsReconnectMutex.Lock()
		if c.wsConn != nil {
			c.wsConn.Close()
			c.wsConn = nil
		}
		c.wsIsConnected = false
		c.wsReconnectMutex.Unlock()
		log.Printf("WebSocket reader stopped for queue: %s", queue)
		// Only schedule reconnect if not explicitly stopped
		select {
		case <-c.wsStopChan:
			log.Println("WebSocket stopped, not reconnecting.")
		default:
			c.scheduleReconnect(queue, pubKey)
		}
	}()

	c.wsReconnectMutex.Lock()
	conn := c.wsConn // Get the current connection under lock
	c.wsReconnectMutex.Unlock()

	if conn == nil {
		log.Println("wsReader: No active connection.")
		return // Exit if connection is already nil
	}

	for {
		// Check for stop signal before reading
		select {
		case <-c.wsStopChan:
			log.Println("wsReader: Stop signal received, exiting.")
			return
		default:
			// Proceed with reading
		}

		var msg WsMessage
		err := conn.ReadJSON(&msg)
		if err != nil {
			// Check if the error is due to a closed connection
			if websocket.IsUnexpectedCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				log.Printf("WebSocket read error (likely closed): %v", err)
			} else if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				log.Println("WebSocket closed normally.")
			} else {
				log.Printf("WebSocket read error: %v", err)
			}
			// Any read error means the connection is likely dead, break to trigger reconnect
			return
		}

		switch msg.Type {
		case "message":
			if c.onMessage != nil {
				c.onMessage(msg.ID, msg.Message)
			} else {
				fmt.Printf("Received: %s\n", formatReceived(msg.Message, pubKey)) // Print message to stdout
			}
		case "pong":
			log.Println("Received PONG")
		case "error":
			log.Printf("Error from server: %s", msg.Message)
		default:
			log.Printf("Received unknown WS message type: %s", msg.Type)
		}
	}
}

func (c *ShadowClient) wsHeartbeat(queue string) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			c.wsReconnectMutex.Lock()
			conn := c.wsConn
			isConnected := c.wsIsConnected
			c.wsReconnectMutex.Unlock()

			if isConnected && conn != nil {
				pingMsg := WsMessage{Type: "ping"}
				// Set a write deadline to prevent blocking indefinitely
				deadline := time.Now().Add(5 * time.Second)
				conn.SetWriteDeadline(deadline)
				err := conn.WriteJSON(pingMsg)
				conn.SetWriteDeadline(time.Time{}) // Clear deadline

				if err != nil {
					log.Printf("Heartbeat ping failed: %v", err)
					// Don't close connection here, let the reader handle connection errors
				} else {
					log.Println("Sent PING")
				}
			}
		case <-c.wsStopChan:
			log.Println("Heartbeat stopped.")
			return
		}
	}
}

func (c *ShadowClient) scheduleReconnect(queue string, pubKey ed25519.PublicKey) {
	c.wsReconnectMutex.Lock()
	defer c.wsReconnectMutex.Unlock() // Ensure unlock even if stopChan is closed
	c.scheduleReconnectLocked(queue, pubKey)
}

// scheduleReconnectLocked schedules a reconnect attempt. The caller must
// already hold wsReconnectMutex (connectWebSocket calls this from its error
// paths while holding the lock).
func (c *ShadowClient) scheduleReconnectLocked(queue string, pubKey ed25519.PublicKey) {
	// Check if stopped before scheduling
	select {
	case <-c.wsStopChan:
		log.Println("Stop signal received, not scheduling reconnect.")
		return
	default:
	}

	if c.wsIsConnected {
		c.wsIsConnected = false // Mark as disconnected if we are scheduling a reconnect
		if c.wsConn != nil {
			c.wsConn.Close() // Attempt to close the old connection cleanly
			c.wsConn = nil
		}
		log.Printf("WebSocket closed for queue: %s", queue)
		go c.notifyState(false)
	}

	if c.wsReconnectAttempts < c.wsMaxReconnects {
		c.wsReconnectAttempts++
		jitter := time.Duration(mrand.Intn(1000)) * time.Millisecond
		delay := c.wsReconnectDelay + jitter
		log.Printf("Attempting reconnect (%d/%d) in %v...", c.wsReconnectAttempts, c.wsMaxReconnects, delay)

		// Schedule the connection attempt in a separate goroutine
		// to avoid blocking the mutex for the duration of the delay.
		time.AfterFunc(delay, func() {
			c.connectWebSocket(queue, pubKey) // Call connectWebSocket, which will acquire the lock
		})

		// Exponential backoff
		c.wsReconnectDelay = time.Duration(float64(c.wsReconnectDelay) * 1.5)
		if c.wsReconnectDelay > 30*time.Second {
			c.wsReconnectDelay = 30 * time.Second
		}
	} else {
		log.Printf("Max reconnect attempts (%d) reached for queue: %s. Stopping.", c.wsMaxReconnects, queue)
		close(c.wsStopChan) // Signal stop as max attempts reached
	}
}

// Ping checks connectivity to the broker and returns the round-trip latency
// of the WS ping/pong exchange.
func (c *ShadowClient) Ping() (bool, time.Duration, error) {
	pingQueue := "_ping" // Special queue for ping
	wsUrl, err := c.makeWsUrl(pingQueue)
	if err != nil {
		return false, 0, fmt.Errorf("error creating WebSocket URL for ping: %w", err)
	}

	log.Println("Attempting ping connection...")
	conn, _, err := websocket.DefaultDialer.Dial(wsUrl, nil)
	if err != nil {
		return false, 0, fmt.Errorf("ping WS dial error: %w", err)
	}
	defer conn.Close()
	log.Println("Ping WS opened, sending auth and ping...")

	// Send auth first
	authMsg := WsMessage{Type: "token", Token: c.config.AuthToken}
	if err := conn.WriteJSON(authMsg); err != nil {
		return false, 0, fmt.Errorf("failed to send auth token during ping: %w", err)
	}

	// Then send ping, measuring the round-trip time from here
	pingMsg := WsMessage{Type: "ping"}
	start := time.Now()
	if err := conn.WriteJSON(pingMsg); err != nil {
		return false, 0, fmt.Errorf("failed to send ping message: %w", err)
	}

	// Wait for pong with timeout
	pongReceived := make(chan bool, 1)
	go func() {
		defer close(pongReceived)
		for {
			var msg WsMessage
			// Set a read deadline
			conn.SetReadDeadline(time.Now().Add(6 * time.Second)) // Slightly longer than timeout
			err := conn.ReadJSON(&msg)
			conn.SetReadDeadline(time.Time{}) // Clear deadline
			if err != nil {
				log.Printf("Ping WS read error: %v", err)
				pongReceived <- false
				return
			}
			if msg.Type == "pong" {
				log.Println("Received PONG on ephemeral WS")
				pongReceived <- true
				return
			}
		}
	}()

	select {
	case success := <-pongReceived:
		return success, time.Since(start), nil
	case <-time.After(5 * time.Second):
		log.Println("Ping timed out after 5s")
		return false, time.Since(start), errors.New("ping timed out")
	}
}

func printUsage() {
	fmt.Print(`
Usage:
  sb <command> [arguments] [--json]

Commands:
  send <queue> <message> [--sign --key <file|base64> --from <name>]
                                   - Publish a message to a queue (optionally as a signed envelope)
  pull <queue> [since] [limit] [--verify-pubkey <file|base64>]
                                   - Pull messages from a queue (default since=0, limit=100)
  subscribe <queue> [--verify-pubkey <file|base64>]
                                   - Subscribe to a queue via WebSocket (Ctrl+C to exit)
  tui [queue] [--key <file|base64> --from <name>] [--verify-pubkey <file|base64>]
                                   - Interactive terminal UI (default queue "general");
                                     type /queue <name> to switch queues, Esc to quit
  test [queue]                     - End-to-end broker self-test (ping, signed send,
                                     WS subscribe + verify, pull); exit 0 when all pass
  keygen [-out <prefix>]           - Generate an Ed25519 keypair (writes <prefix> and <prefix>.pub with -out)
  ping                             - Test connectivity to the broker
  help                             - Show this usage

Global flags:
  --json                           - Machine-readable output (one JSON object per line);
                                     works in any argument position

Environment variable required:
  SHADOW_BROKER_CONFIG: base64-encoded JSON string containing { "url": "...", "auth_token": "..." }
`)
}

// runKeygen generates an Ed25519 keypair and prints it, optionally writing
// the keys to <prefix> (private, 0600) and <prefix>.pub (public, 0644).
func runKeygen(args []string) {
	out := ""
	if len(args) > 0 {
		if len(args) == 2 && args[0] == "-out" {
			out = args[1]
		} else {
			log.Fatal("Usage: sb keygen [-out <prefix>]")
		}
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		log.Fatalf("Failed to generate keypair: %v", err)
	}
	privB64 := base64.StdEncoding.EncodeToString(priv)
	pubB64 := base64.StdEncoding.EncodeToString(pub)

	if out != "" {
		if err := os.WriteFile(out, []byte(privB64+"\n"), 0600); err != nil {
			log.Fatalf("Failed to write private key: %v", err)
		}
		if err := os.WriteFile(out+".pub", []byte(pubB64+"\n"), 0644); err != nil {
			log.Fatalf("Failed to write public key: %v", err)
		}
		fmt.Printf("Private key written to %s (mode 0600)\n", out)
		fmt.Printf("Public key written to %s (mode 0644)\n", out+".pub")
	}

	fmt.Printf("Private key (base64, keep secret): %s\n", privB64)
	fmt.Printf("Public key (base64): %s\n", pubB64)
	fmt.Println("\nSign messages with:    sb send <queue> <message> --sign --key <private key file|base64> --from <name>")
	fmt.Println("Verify messages with:  sb subscribe <queue> --verify-pubkey <public key file|base64>")
}

func main() {
	// Seed random number generator (for reconnect jitter)
	mrand.Seed(time.Now().UnixNano())

	// Arguments are parsed manually (extractFlag/extractBoolFlag) instead of
	// the flag package so that switches like --json work in any position,
	// including before the command name.
	args := os.Args[1:]
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		printUsage()
		return
	}
	// --json is a global flag and may appear in any argument position.
	args, jsonOut := extractBoolFlag(args, "json")
	if len(args) < 1 {
		printUsage()
		os.Exit(1)
	}

	cmd := args[0]

	// keygen works locally and does not need the broker config
	if cmd == "keygen" {
		runKeygen(args[1:])
		return
	}

	config, err := loadConfig()
	if err != nil {
		log.Fatalf("Configuration error: %v", err)
	}

	client := NewShadowClient(config)

	switch cmd {
	case "send":
		rest := args[1:]
		rest, doSign := extractBoolFlag(rest, "sign")
		keyRef, rest, _ := extractFlag(rest, "key")
		fromName, rest, _ := extractFlag(rest, "from")
		// "--key" without an explicit "--sign" also enables signing
		if keyRef != "" && !doSign {
			doSign = true
		}
		if len(rest) != 2 {
			log.Fatal("Usage: sb send <queue> <message> [--sign --key <file|base64> --from <name>]")
		}
		queue, message := rest[0], rest[1]
		if doSign {
			if keyRef == "" {
				log.Fatal("Signing requires --key <file|base64>")
			}
			priv, err := loadKey(keyRef, ed25519.PrivateKeySize, "private")
			if err != nil {
				log.Fatalf("Key error: %v", err)
			}
			message, err = signEnvelope(message, fromName, priv)
			if err != nil {
				log.Fatalf("Signing error: %v", err)
			}
		}
		id, err := client.Publish(queue, message)
		if err != nil {
			log.Fatalf("Error publishing: %v", err)
		}
		if jsonOut {
			fmt.Printf("{\"id\":%d}\n", id)
		}
	case "pull":
		rest := args[1:]
		pubRef, rest, doVerify := extractFlag(rest, "verify-pubkey")
		if len(rest) < 1 {
			log.Fatal("Usage: sb pull <queue> [since] [limit] [--verify-pubkey <file|base64>]")
		}
		var pubKey ed25519.PublicKey
		if doVerify {
			key, err := loadKey(pubRef, ed25519.PublicKeySize, "public")
			if err != nil {
				log.Fatalf("Key error: %v", err)
			}
			pubKey = key
		}
		queue := rest[0]
		var since int64 = 0
		var limit int = 100
		if len(rest) > 1 {
			since, err = strconv.ParseInt(rest[1], 10, 64)
			if err != nil {
				log.Fatalf("Invalid 'since' value: %v", err)
			}
		}
		if len(rest) > 2 {
			limit, err = strconv.Atoi(rest[2])
			if err != nil {
				log.Fatalf("Invalid 'limit' value: %v", err)
			}
		}
		result, err := client.Pull(queue, since, limit)
		if err != nil {
			log.Fatalf("Error pulling: %v", err)
		}
		if jsonOut {
			messages := result.Messages
			if messages == nil {
				messages = []PullMessage{}
			}
			rawJSON, err := json.Marshal(messages)
			if err != nil {
				log.Fatalf("Failed to encode messages as JSON: %v", err)
			}
			fmt.Println(string(rawJSON))
			break
		}
		for _, msg := range result.Messages {
			fmt.Printf("[ID=%d] %s (created_at: %s)\n", msg.ID, formatReceived(msg.Message, pubKey), msg.CreatedAt)
		}
		fmt.Printf("Last ID: %d\n", result.LastID)

	case "subscribe":
		rest := args[1:]
		pubRef, rest, doVerify := extractFlag(rest, "verify-pubkey")
		if len(rest) != 1 {
			log.Fatal("Usage: sb subscribe <queue> [--verify-pubkey <file|base64>]")
		}
		var pubKey ed25519.PublicKey
		if doVerify {
			key, err := loadKey(pubRef, ed25519.PublicKeySize, "public")
			if err != nil {
				log.Fatalf("Key error: %v", err)
			}
			pubKey = key
		}
		queue := rest[0]
		if jsonOut {
			// In --json mode each incoming message is printed as one compact
			// JSON object instead of the human-readable rendering.
			client.onMessage = func(id int64, raw string) {
				fmt.Println(formatReceivedJSON(id, raw, pubKey))
			}
		}
		log.Printf("Subscribing to queue: %s - press Ctrl+C to exit.", queue)
		err := client.Subscribe(queue, pubKey) // This function blocks until error or signal
		if err != nil {
			log.Fatalf("Subscription error: %v", err) // Should ideally not be reached if Subscribe handles exits
		}

	case "test":
		rest := args[1:]
		if len(rest) > 1 {
			log.Fatal("Usage: sb test [queue] [--json]")
		}
		queue := ""
		if len(rest) == 1 {
			queue = rest[0]
		}
		report := runSelfTest(client, queue)
		printSelfTestReport(report, jsonOut)
		if !report.OK {
			os.Exit(1)
		}

	case "tui":
		rest := args[1:]
		keyRef, rest, _ := extractFlag(rest, "key")
		fromName, rest, _ := extractFlag(rest, "from")
		pubRef, rest, doVerify := extractFlag(rest, "verify-pubkey")
		if len(rest) > 1 {
			log.Fatal("Usage: sb tui [queue] [--key <file|base64> --from <name>] [--verify-pubkey <file|base64>]")
		}
		queue := "general"
		if len(rest) == 1 {
			queue = rest[0]
		}
		var priv ed25519.PrivateKey
		if keyRef != "" {
			key, err := loadKey(keyRef, ed25519.PrivateKeySize, "private")
			if err != nil {
				log.Fatalf("Key error: %v", err)
			}
			priv = key
		}
		var pubKey ed25519.PublicKey
		if doVerify {
			key, err := loadKey(pubRef, ed25519.PublicKeySize, "public")
			if err != nil {
				log.Fatalf("Key error: %v", err)
			}
			pubKey = key
		}
		if err := runTUI(config, queue, priv, fromName, pubKey); err != nil {
			log.Fatalf("TUI error: %v", err)
		}

	case "ping":
		if len(args) != 1 {
			log.Fatal("Usage: sb ping [--json]")
		}
		success, latency, err := client.Ping()
		if err != nil {
			log.Fatalf("Ping error: %v", err) // Already logged timeout internally
		}
		if jsonOut {
			fmt.Printf("{\"ok\":%t,\"latency_ms\":%d}\n", success, latency.Milliseconds())
		} else {
			fmt.Printf("Ping result: %t (latency: %d ms)\n", success, latency.Milliseconds())
		}
		if !success {
			os.Exit(1)
		}

	case "help":
		printUsage()

	default:
		log.Printf("Unknown command: %s", cmd)
		printUsage()
		os.Exit(1)
	}
}
