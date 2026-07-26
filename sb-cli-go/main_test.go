package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestEnvelopeRoundtrip(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	raw, err := signEnvelope("hello world", "agent-a", priv)
	if err != nil {
		t.Fatalf("signEnvelope: %v", err)
	}

	// Envelope must be compact JSON with alphabetically ordered keys.
	if !strings.HasPrefix(raw, `{"body":"hello world","from":"agent-a","sig":"`) {
		t.Errorf("envelope is not in canonical compact form: %s", raw)
	}
	if !strings.HasSuffix(raw, `,"v":1}`) {
		t.Errorf("envelope missing trailing fields: %s", raw)
	}

	env, isEnvelope, valid := parseEnvelope(raw, pub)
	if !isEnvelope {
		t.Fatal("signed message not recognized as envelope")
	}
	if !valid {
		t.Fatal("valid signature rejected")
	}
	if env.From != "agent-a" || env.Body != "hello world" || env.Ts == 0 || env.V != 1 {
		t.Errorf("unexpected envelope contents: %+v", env)
	}
}

func TestEnvelopeTampered(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	raw, err := signEnvelope("hello world", "agent-a", priv)
	if err != nil {
		t.Fatalf("signEnvelope: %v", err)
	}

	// Tamper with the body, keeping the original signature.
	tampered := strings.Replace(raw, "hello world", "forged body", 1)
	env, isEnvelope, valid := parseEnvelope(tampered, pub)
	if !isEnvelope {
		t.Fatal("tampered message should still parse as an envelope")
	}
	if valid {
		t.Fatal("tampered body verified as valid")
	}

	// Tampered sender must also fail.
	tamperedFrom := strings.Replace(raw, `"agent-a"`, `"agent-b"`, 1)
	_, isEnvelope, valid = parseEnvelope(tamperedFrom, pub)
	if !isEnvelope || valid {
		t.Fatal("tampered sender verified as valid")
	}
	_ = env
}

func TestEnvelopeWrongKey(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	otherPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	raw, err := signEnvelope("hello", "agent-a", priv)
	if err != nil {
		t.Fatalf("signEnvelope: %v", err)
	}

	_, isEnvelope, valid := parseEnvelope(raw, otherPub)
	if !isEnvelope {
		t.Fatal("message should parse as an envelope")
	}
	if valid {
		t.Fatal("signature verified against the wrong public key")
	}
}

func TestCanonicalPayloadSpec(t *testing.T) {
	got := string(canonicalPayload("agent-a", 1712345678, "hi"))
	want := "v1\nagent-a\n1712345678\nhi"
	if got != want {
		t.Errorf("canonical payload mismatch:\n got: %q\nwant: %q", got, want)
	}
}

func TestParseEnvelopeNonEnvelope(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	for _, raw := range []string{
		"plain text message",
		`{"some":"other json"}`,
		`{"v":2,"from":"x","sig":"AAAA","body":"y","ts":1}`,
	} {
		if _, isEnvelope, _ := parseEnvelope(raw, pub); isEnvelope {
			t.Errorf("non-envelope message detected as envelope: %q", raw)
		}
	}
}

func TestLoadKey(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	b64 := base64.StdEncoding.EncodeToString(priv)

	// Direct base64 form.
	key, err := loadKey(b64, ed25519.PrivateKeySize, "private")
	if err != nil {
		t.Fatalf("loadKey (base64): %v", err)
	}
	if !ed25519.PrivateKey(key).Equal(priv) {
		t.Fatal("loaded key differs from original")
	}

	// File form.
	path := t.TempDir() + "/agent.key"
	if err := writeTestKeyFile(path, b64); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	key, err = loadKey(path, ed25519.PrivateKeySize, "private")
	if err != nil {
		t.Fatalf("loadKey (file): %v", err)
	}
	if !ed25519.PrivateKey(key).Equal(priv) {
		t.Fatal("loaded key from file differs from original")
	}

	// Wrong size must fail.
	if _, err := loadKey(base64.StdEncoding.EncodeToString(priv[:32]), ed25519.PrivateKeySize, "private"); err == nil {
		t.Fatal("expected error for wrong key size")
	}
}

func TestExtractFlags(t *testing.T) {
	args := []string{"q", "msg", "--sign", "--key", "k", "--from=a"}
	args, sign := extractBoolFlag(args, "sign")
	if !sign {
		t.Fatal("--sign not detected")
	}
	key, args, found := extractFlag(args, "key")
	if !found || key != "k" {
		t.Fatalf("--key not detected: found=%v key=%q", found, key)
	}
	from, args, found := extractFlag(args, "from")
	if !found || from != "a" {
		t.Fatalf("--from not detected: found=%v from=%q", found, from)
	}
	if len(args) != 2 || args[0] != "q" || args[1] != "msg" {
		t.Errorf("unexpected remaining args: %v", args)
	}
}

// Regression test: extracting a flag pair must not corrupt the extracted
// value when two or more arguments follow it (previously the returned slice
// shared the backing array and append overwrote args[i+1] before it was
// read).
func TestExtractFlagValueWithTrailingArgs(t *testing.T) {
	args := []string{"q", "msg", "--sign", "--key", "/tmp/key", "--from", "agent-x"}
	args, _ = extractBoolFlag(args, "sign")
	key, args, found := extractFlag(args, "key")
	if !found || key != "/tmp/key" {
		t.Fatalf("--key value corrupted: found=%v key=%q", found, key)
	}
	from, args, found := extractFlag(args, "from")
	if !found || from != "agent-x" {
		t.Fatalf("--from not detected: found=%v from=%q", found, from)
	}
	if len(args) != 2 || args[0] != "q" || args[1] != "msg" {
		t.Errorf("unexpected remaining args: %v", args)
	}

	// Same for the --name=value form.
	args = []string{"q", "msg", "--key=/tmp/key", "extra1", "extra2"}
	key, args, found = extractFlag(args, "key")
	if !found || key != "/tmp/key" {
		t.Fatalf("--key= value corrupted: found=%v key=%q", found, key)
	}
	if len(args) != 4 {
		t.Errorf("unexpected remaining args: %v", args)
	}

	// And for the boolean form.
	args = []string{"q", "msg", "--json", "extra1", "extra2"}
	args, found = extractBoolFlag(args, "json")
	if !found || len(args) != 4 || args[2] != "extra1" || args[3] != "extra2" {
		t.Errorf("unexpected args after --json removal: found=%v args=%v", found, args)
	}
}

func TestFormatReceivedJSON(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	raw, err := signEnvelope("hello world", "agent-a", priv)
	if err != nil {
		t.Fatalf("signEnvelope: %v", err)
	}

	// Envelope with a verification key: body is unwrapped, from/verified set.
	line := formatReceivedJSON(42, raw, pub)
	var got map[string]interface{}
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v (%s)", err, line)
	}
	if got["id"].(float64) != 42 || got["message"] != "hello world" || got["from"] != "agent-a" || got["verified"] != true {
		t.Errorf("unexpected JSON: %s", line)
	}

	// Envelope without a verification key: from set, verified absent.
	line = formatReceivedJSON(7, raw, nil)
	got = map[string]interface{}{}
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v (%s)", err, line)
	}
	if got["from"] != "agent-a" || got["message"] != "hello world" {
		t.Errorf("unexpected JSON without key: %s", line)
	}
	if _, present := got["verified"]; present {
		t.Errorf("verified must be absent without a public key: %s", line)
	}

	// Tampered envelope must verify as false.
	tampered := strings.Replace(raw, "hello world", "forged body", 1)
	line = formatReceivedJSON(1, tampered, pub)
	got = map[string]interface{}{}
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v (%s)", err, line)
	}
	if got["verified"] != false {
		t.Errorf("tampered envelope verified as true: %s", line)
	}

	// Plain message: only id and message.
	line = formatReceivedJSON(9, "plain text", pub)
	got = map[string]interface{}{}
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v (%s)", err, line)
	}
	if got["id"].(float64) != 9 || got["message"] != "plain text" || len(got) != 2 {
		t.Errorf("unexpected JSON for plain message: %s", line)
	}
}

func TestRandomQueueName(t *testing.T) {
	name := randomQueueName()
	if !strings.HasPrefix(name, "_sbtest-") || len(name) != len("_sbtest-")+8 {
		t.Errorf("unexpected queue name: %q", name)
	}
	if randomQueueName() == name {
		t.Error("queue names are not random")
	}
}

func TestTUIHandleInputQueueSwitch(t *testing.T) {
	config := Config{URL: "http://localhost:1/", AuthToken: "x"}
	m := newTUIModel(config, "general", nil, "", nil)
	defer m.stopClient()

	// /queue without a name only sets a notice.
	if cmd := m.handleInput("/queue"); cmd != nil {
		t.Fatal("expected no command for bare /queue")
	}
	if m.notice == "" || m.queue != "general" {
		t.Errorf("bare /queue: notice=%q queue=%q", m.notice, m.queue)
	}

	// /queue <name> switches the queue.
	m.handleInput("/queue alerts")
	if m.queue != "alerts" {
		t.Errorf("queue not switched: %q", m.queue)
	}
	if len(m.lines) != 1 || !strings.Contains(m.lines[0], "alerts") {
		t.Errorf("switch marker line missing: %v", m.lines)
	}

	// Empty input is ignored.
	if cmd := m.handleInput("   "); cmd != nil {
		t.Fatal("expected no command for empty input")
	}
}

func TestTUIHandleInputSend(t *testing.T) {
	config := Config{URL: "http://localhost:1/", AuthToken: "x"}
	m := newTUIModel(config, "general", nil, "", nil)
	defer m.stopClient()

	// Sending produces a command whose publish fails fast (port 1 is closed),
	// proving the send path is wired without needing a live broker.
	cmd := m.handleInput("hello")
	if cmd == nil {
		t.Fatal("expected a send command")
	}
	msg := cmd()
	res, ok := msg.(sendResultMsg)
	if !ok {
		t.Fatalf("unexpected message type: %T", msg)
	}
	if res.err == nil {
		t.Fatal("expected publish error against a closed port")
	}
}

func writeTestKeyFile(path, b64 string) error {
	return os.WriteFile(path, []byte(b64+"\n"), 0600)
}
