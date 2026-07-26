package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// ======================= CONFIG AND STRUCTURES =======================

// wsClient wraps a WS connection with a per-connection write mutex.
// gorilla/websocket does not support concurrent writes, so every write
// (broadcasts, pongs) must go through writeMessage.
type wsClient struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
}

// writeMessage sends a text message with a write deadline, serialized per client.
func (c *wsClient) writeMessage(payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return c.conn.WriteMessage(websocket.TextMessage, payload)
}

type BrokerConfig struct {
	URL            string `json:"url"`              // https://my-broker.fly.dev
	SecretKey      string `json:"secret_key"`       // 32 bytes
	AuthToken      string `json:"auth_token"`       // For API authentication
	DBUrl          string `json:"db_url"`           // optional: file:... (local, default) or libsql://... (Turso)
	TursoAuthToken string `json:"turso_auth_token"` // optional: Turso database auth token
}

var (
	gConf    BrokerConfig
	store    messageStore
	logger   *log.Logger
	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}

	// Instead of local queues - we only have a map of subscribers (ws per queue)
	clients   = make(map[string]map[*wsClient]bool) // queue -> set of WS clients
	clientsMu sync.Mutex

	// Rate limit
	recentReqMap = make(map[string][]time.Time)
	rateLimitMu  sync.Mutex
	maxReqPer10s = 500

	// Poller
	globalMaxID int64 = 0 // for detecting new messages in DB
	pollMu      sync.Mutex
)

// ======================= CORS MIDDLEWARE =======================

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Set CORS headers
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Origin, X-Requested-With, Content-Type, Accept, X-Auth, Authorization")

		// Handle preflight OPTIONS request
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// ======================= DATABASE: writes and reads =======================
// All database access goes through the messageStore interface (see db.go);
// the default backend is an embedded SQLite file, Turso remains an option.

// ======================= PUBLISH / WS / RATE-LIMIT / PULL =======================
func publishHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	auth := r.Header.Get("X-Auth")
	if auth != gConf.AuthToken {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	type pubReq struct {
		Queue   string `json:"queue"`
		Message string `json:"message"`
	}
	var req pubReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.Queue == "" || req.Message == "" {
		http.Error(w, "queue and message required", http.StatusBadRequest)
		return
	}

	// Encrypt
	ciph, nonce, err := encryptAESGCM([]byte(req.Message), []byte(gConf.SecretKey))
	if err != nil {
		http.Error(w, "encryption error", http.StatusInternalServerError)
		return
	}
	nonceB64 := base64.StdEncoding.EncodeToString(nonce)
	ciphB64 := base64.StdEncoding.EncodeToString(ciph)

	msgID, err := store.Insert(req.Queue, nonceB64, ciphB64)
	if err != nil {
		http.Error(w, "DB insert error", http.StatusInternalServerError)
		return
	}

	logger.Printf("PUBLISH queue=%s id=%d\n", req.Queue, msgID)
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(fmt.Sprintf(`{"id":%d}`, msgID)))
}

func pullHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	auth := r.Header.Get("X-Auth")
	if auth != gConf.AuthToken {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	queue := r.URL.Query().Get("queue")
	if queue == "" {
		http.Error(w, "queue param required", http.StatusBadRequest)
		return
	}

	sinceStr := r.URL.Query().Get("since")
	var sinceID int64 = 0
	if sinceStr != "" {
		var err error
		sinceID, err = strconv.ParseInt(sinceStr, 10, 64)
		if err != nil {
			http.Error(w, "invalid since parameter", http.StatusBadRequest)
			return
		}
	}

	limitStr := r.URL.Query().Get("limit")
	var limit int = 100 // Default limit
	if limitStr != "" {
		var err error
		limit, err = strconv.Atoi(limitStr)
		if err != nil || limit <= 0 || limit > 1000 {
			http.Error(w, "invalid limit (must be 1-1000)", http.StatusBadRequest)
			return
		}
	}

	// Fetch messages from the database
	rows, err := store.Pull(queue, sinceID, limit)
	if err != nil {
		logger.Printf("ERROR pull query: %v\n", err)
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}

	var messages []map[string]interface{}
	var lastID int64 = sinceID

	for _, row := range rows {
		// Decrypt the message
		nonce, err := base64.StdEncoding.DecodeString(row.NonceB64)
		if err != nil {
			logger.Printf("WARNING pull: bad nonce base64 id=%d: %v\n", row.ID, err)
			continue
		}
		ciph, err := base64.StdEncoding.DecodeString(row.CipherB64)
		if err != nil {
			logger.Printf("WARNING pull: bad ciphertext base64 id=%d: %v\n", row.ID, err)
			continue
		}
		plaintext, err := decryptAESGCM(ciph, nonce, []byte(gConf.SecretKey))
		if err != nil {
			logger.Printf("WARNING pull: decrypt failed id=%d: %v\n", row.ID, err)
			continue
		}

		messages = append(messages, map[string]interface{}{
			"id":         row.ID,
			"message":    string(plaintext),
			"created_at": row.CreatedAt,
		})

		if row.ID > lastID {
			lastID = row.ID
		}
	}

	// Prepare response
	response := map[string]interface{}{
		"messages": messages,
		"last_id":  lastID,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
	logger.Printf("PULL queue=%s count=%d since=%d\n", queue, len(messages), sinceID)
}

func wsHandler(w http.ResponseWriter, r *http.Request) {
	queue := r.URL.Query().Get("queue")
	if queue == "" {
		http.Error(w, "queue param required", http.StatusBadRequest)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	// Read auth token in the first message
	_, msg, err := conn.ReadMessage()
	if err != nil {
		log.Printf("WS handshake read error: %v\n", err)
		conn.Close()
		return
	}
	var handshake struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(msg, &handshake); err != nil {
		log.Printf("WS handshake JSON parse error: %v\n", err)
		conn.Close()
		return
	}

	if handshake.Token != gConf.AuthToken {
		log.Printf("WS handshake rejected: invalid token (remote=%s)\n", r.RemoteAddr)
		conn.Close()
		return
	}

	client := &wsClient{conn: conn}
	logger.Printf("WS CONNECT queue=%s remote=%s\n", queue, r.RemoteAddr)
	clientsMu.Lock()
	if clients[queue] == nil {
		clients[queue] = make(map[*wsClient]bool)
	}
	clients[queue][client] = true
	clientsMu.Unlock()

	go func() {
		defer func() {
			clientsMu.Lock()
			delete(clients[queue], client)
			clientsMu.Unlock()
			conn.Close()
			logger.Printf("WS DISCONNECT queue=%s remote=%s\n", queue, r.RemoteAddr)
		}()
		for {
			mt, msg, err := conn.ReadMessage()
			if err != nil {
				break
			}
			if mt == websocket.TextMessage {
				var raw map[string]interface{}
				if err := json.Unmarshal(msg, &raw); err == nil {
					if raw["type"] == "ping" {
						pong := map[string]string{"type": "pong"}
						pongData, _ := json.Marshal(pong)
						if err := client.writeMessage(pongData); err != nil {
							logger.Printf("WS pong error queue=%s remote=%s: %v\n", queue, r.RemoteAddr, err)
							break
						}
					}
				}
			}
		}
	}()
}

func rateLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip, _, _ := net.SplitHostPort(r.RemoteAddr)
		now := time.Now()

		rateLimitMu.Lock()
		arr := recentReqMap[ip]
		arr = append(arr, now)

		cutoff := now.Add(-10 * time.Second)
		filtered := make([]time.Time, 0, len(arr))
		for _, t := range arr {
			if t.After(cutoff) {
				filtered = append(filtered, t)
			}
		}
		recentReqMap[ip] = filtered
		count := len(filtered)
		rateLimitMu.Unlock()

		if count > maxReqPer10s {
			http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// startRateLimitJanitor - periodically removes stale IPs from the rate-limit map
// so it does not grow unbounded.
func startRateLimitJanitor() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		cutoff := time.Now().Add(-10 * time.Second)
		rateLimitMu.Lock()
		for ip, arr := range recentReqMap {
			// timestamps are appended in time order; the last one is the most recent
			if len(arr) == 0 || !arr[len(arr)-1].After(cutoff) {
				delete(recentReqMap, ip)
			}
		}
		rateLimitMu.Unlock()
	}
}

// ======================= POLLER - REFRESHING MESSAGES =======================

// initGlobalMaxID - initialize globalMaxID with the current MAX(id) so the poller
// does not replay the whole table history after a restart.
// (History remains available via /pull.) Falls back to 0 on persistent DB errors.
func initGlobalMaxID() {
	const maxRetries = 5
	var lastErr error

	for attempt := 0; attempt <= maxRetries; attempt++ {
		maxID, err := store.MaxID()
		if err == nil {
			globalMaxID = maxID
			logger.Printf("Poller will start from id=%d\n", maxID)
			return
		}

		lastErr = err
		logger.Printf("DB max(id) query error (attempt %d/%d): %v\n", attempt+1, maxRetries+1, err)
		if attempt < maxRetries {
			time.Sleep(time.Duration(100*(attempt+1)) * time.Millisecond)
		}
	}

	logger.Printf("WARNING: could not read MAX(id), poller starting from 0: %v\n", lastErr)
}

func startPoller() {
	logger.Printf("Poller started (from id=%d)\n", globalMaxID)
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		<-ticker.C
		pollMu.Lock()
		newMessages, err := store.FetchNew(globalMaxID)
		if err != nil {
			pollMu.Unlock()
			logger.Printf("ERROR fetchNewMessages: %v\n", err)
			continue
		}
		if len(newMessages) > 0 {
			logger.Printf("Polled %d new messages\n", len(newMessages))
			// Group messages by queue
			queueMessages := make(map[string][]DBMessage)
			for _, row := range newMessages {
				if row.ID > globalMaxID {
					globalMaxID = row.ID
				}
				queueMessages[row.Queue] = append(queueMessages[row.Queue], row)
			}

			// Process each queue's messages sequentially in order
			for _, messages := range queueMessages {
				// Sort by ID to ensure correct order
				sort.Slice(messages, func(i, j int) bool {
					return messages[i].ID < messages[j].ID
				})

				for _, msg := range messages {
					// Process synchronously to maintain order within each queue
					broadcastRow(msg)
				}
			}
		}
		pollMu.Unlock()
	}
}

// broadcastRow - decrypts and sends to subscribers in the given queue
func broadcastRow(row DBMessage) {
	nonce, _ := base64.StdEncoding.DecodeString(row.NonceB64)
	ciph, _ := base64.StdEncoding.DecodeString(row.CipherB64)
	plaintext, err := decryptAESGCM(ciph, nonce, []byte(gConf.SecretKey))
	if err != nil {
		logger.Printf("ERROR decrypt id=%d: %v\n", row.ID, err)
		return
	}

	msg := map[string]interface{}{
		"type":    "message",
		"id":      row.ID,
		"message": string(plaintext),
	}
	payload, _ := json.Marshal(msg)

	// Snapshot subscribers under the lock, then write without holding it
	// so one slow client does not block the whole broadcast.
	clientsMu.Lock()
	subs := make([]*wsClient, 0, len(clients[row.Queue]))
	for c := range clients[row.Queue] {
		subs = append(subs, c)
	}
	clientsMu.Unlock()

	for _, c := range subs {
		if err := c.writeMessage(payload); err != nil {
			logger.Printf("WS send error to id=%d: %v\n", row.ID, err)
			c.conn.Close()
			clientsMu.Lock()
			delete(clients[row.Queue], c)
			clientsMu.Unlock()
		}
	}
}

// ======================= AES-GCM ENCRYPTION =======================
func encryptAESGCM(plaintext, key []byte) (ciphertext, nonce []byte, err error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, err
	}
	nonce = make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, err
	}
	ciphertext = gcm.Seal(nil, nonce, plaintext, nil)
	return ciphertext, nonce, nil
}

func decryptAESGCM(ciphertext, nonce, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, nonce, ciphertext, nil)
}

// ======================= MAIN =======================
func main() {
	// genconfig version (optional)
	if len(os.Args) > 1 && os.Args[1] == "genconfig" {
		if len(os.Args) < 5 {
			fmt.Println("Usage: go run main.go genconfig <URL> <SECRET_32B> <AUTH_TOKEN> [TURSO_DB_URL] [TURSO_AUTH_TOKEN]")
			fmt.Println("Without the Turso arguments the broker uses an embedded SQLite file (" + defaultLocalDBUrl + ").")
			os.Exit(1)
		}
		conf := BrokerConfig{
			URL:       os.Args[2],
			SecretKey: os.Args[3],
			AuthToken: os.Args[4],
		}
		if len(os.Args) > 5 {
			conf.DBUrl = os.Args[5]
		}
		if len(os.Args) > 6 {
			conf.TursoAuthToken = os.Args[6]
		}
		if len(conf.SecretKey) != 32 {
			fmt.Println("WARNING: SECRET_KEY should be 32 characters (32 bytes).")
		}
		j, _ := json.Marshal(conf)
		b64 := base64.StdEncoding.EncodeToString(j)
		fmt.Println("SHADOW_BROKER_CONFIG=", b64)
		return
	}

	// 1. Load config from ENV
	fmt.Println("-----------------")
	fmt.Println("Starting shadow broker....")
	fmt.Println("-----------------")
	enc := os.Getenv("SHADOW_BROKER_CONFIG")
	if enc == "" {
		log.Fatalln("Missing SHADOW_BROKER_CONFIG in env")
	}
	dec, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		log.Fatalf("Decode config err: %v\n", err)
	}
	if err := json.Unmarshal(dec, &gConf); err != nil {
		log.Fatalf("JSON unmarshal err: %v\n", err)
	}
	if gConf.URL == "" || gConf.SecretKey == "" || gConf.AuthToken == "" {
		log.Fatalln("Config must include at least: url, secret_key, auth_token (db_url is optional, defaults to a local SQLite file)")
	}

	logger = log.New(os.Stdout, "[ShadowBroker] ", log.LstdFlags)

	// 3. DB initialization (local embedded SQLite by default, Turso optional)
	store, err = newMessageStore(gConf.DBUrl, gConf.TursoAuthToken)
	if err != nil {
		logger.Fatalf("DB init error: %v\n", err)
	}
	dbURL := gConf.DBUrl
	if dbURL == "" {
		dbURL = defaultLocalDBUrl
	}
	logger.Printf("Message store ready (db_url=%s)\n", dbURL)

	// 4. Start the poller (from the current MAX(id) - no history replay on restart)
	initGlobalMaxID()
	go startPoller()

	// 5. Rate-limit map cleanup
	go startRateLimitJanitor()

	// 5. HTTP handlers
	mux := http.NewServeMux()
	mux.HandleFunc("/publish", publishHandler)
	mux.HandleFunc("/ws", wsHandler)
	mux.HandleFunc("/pull", pullHandler)
	// Rate-limit
	wrapped := rateLimitMiddleware(mux)

	// Apply CORS middleware
	wrapped = corsMiddleware(wrapped)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	logger.Printf("ShadowBroker starting on port %s, URL: %s\n", port, gConf.URL)
	srv := &http.Server{
		Addr:    ":" + port,
		Handler: wrapped,
	}
	log.Fatal(srv.ListenAndServe())
}
