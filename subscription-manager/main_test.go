package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDecodeBody(t *testing.T) {
	plain := []byte("vless://uuid@example.test:443\ninvalid\n")
	got := decodeBody(plain)
	if len(got) != 2 || got[1] != "invalid" {
		t.Fatalf("decode plain = %#v", got)
	}
	encoded := base64.StdEncoding.EncodeToString([]byte("vless://uuid@example.test:443\n"))
	got = decodeBody([]byte(encoded))
	if len(got) != 1 || got[0] != "vless://uuid@example.test:443" {
		t.Fatalf("decode base64 = %#v", got)
	}
}

func fixtureManager(t *testing.T, client *http.Client) *Manager {
	t.Helper()
	dir := t.TempDir()
	m := &Manager{base: filepath.Join(dir, "base.json"), output: filepath.Join(dir, "runtime", "config.json"), state: filepath.Join(dir, "state.json"), client: client, validate: func(string) error { return nil }}
	if err := writeJSONAtomic(m.base, map[string]any{"outbounds": []any{map[string]any{"type": "direct", "tag": "direct"}}}); err != nil {
		t.Fatal(err)
	}
	return m
}

func postSubscription(m *Manager, sub Subscription) *httptest.ResponseRecorder {
	body, _ := json.Marshal(sub)
	w := httptest.NewRecorder()
	m.handler(w, httptest.NewRequest(http.MethodPost, "/subscriptions", bytes.NewReader(body)))
	return w
}

func TestFailedRefreshPreservesConfig(t *testing.T) {
	for _, failure := range []string{"http", "empty", "garbage", "mixed", "base64", "port", "transport", "validation"} {
		t.Run(failure, func(t *testing.T) {
			var mu sync.Mutex
			bad := false
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if !bad || r.URL.Path == "/good" {
					fmt.Fprint(w, "trojan://secret@example.test:443")
					return
				}
				switch failure {
				case "http":
					w.WriteHeader(503)
				case "empty":
				case "garbage":
					fmt.Fprint(w, "not a subscription")
				case "mixed":
					fmt.Fprint(w, "trojan://secret@example.test:443\nunsupported://credential@example.test:443")
				case "base64":
					fmt.Fprint(w, base64.StdEncoding.EncodeToString([]byte("trojan://secret@example.test:443\ninvalid")))
				case "port":
					fmt.Fprint(w, "trojan://secret@example.test:65536")
				case "transport":
					fmt.Fprint(w, "trojan://secret@example.test:443?type=xhttp")
				case "validation":
					fmt.Fprint(w, "trojan://secret@example.test:443")
				}
			}))
			defer s.Close()
			m := fixtureManager(t, s.Client())
			if err := writeJSONAtomic(m.state, State{Subscriptions: []Subscription{{"good", s.URL + "/good"}, {"bad", s.URL + "/bad"}}}); err != nil {
				t.Fatal(err)
			}
			if _, err := m.build(); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(m.output)
			mu.Lock()
			bad = true
			mu.Unlock()
			if failure == "validation" {
				m.validate = func(string) error { return errors.New("credential-secret") }
			}
			w := httptest.NewRecorder()
			m.handler(w, httptest.NewRequest(http.MethodPost, "/refresh", nil))
			if w.Code < 400 {
				t.Fatalf("refresh = %d", w.Code)
			}
			after, _ := os.ReadFile(m.output)
			if !bytes.Equal(before, after) {
				t.Fatal("failed refresh changed output")
			}
			w = httptest.NewRecorder()
			m.handler(w, httptest.NewRequest(http.MethodGet, "/health", nil))
			if w.Code != 503 || strings.Contains(w.Body.String(), "secret") {
				t.Fatalf("health = %d %s", w.Code, w.Body)
			}
		})
	}
}

func TestInvalidAdditionDoesNotMutateState(t *testing.T) {
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bad" {
			fmt.Fprint(w, "invalid")
			return
		}
		fmt.Fprint(w, "trojan://secret@example.test:443")
	}))
	defer s.Close()
	m := fixtureManager(t, s.Client())
	if w := postSubscription(m, Subscription{"a.b", s.URL}); w.Code != 200 {
		t.Fatal(w.Body)
	}
	before, _ := os.ReadFile(m.state)
	config, _ := os.ReadFile(m.output)
	for _, sub := range []Subscription{{"bad", s.URL + "/bad"}, {"a b", s.URL}, {"bad", "http://example.test"}} {
		if w := postSubscription(m, sub); w.Code < 400 {
			t.Fatalf("accepted %#v", sub)
		}
		after, _ := os.ReadFile(m.state)
		if !bytes.Equal(before, after) {
			t.Fatal("state changed")
		}
		after, _ = os.ReadFile(m.output)
		if !bytes.Equal(config, after) {
			t.Fatal("config changed")
		}
	}
	m.validate = func(string) error { return errors.New("invalid config") }
	if w := postSubscription(m, Subscription{"new", s.URL}); w.Code < 400 {
		t.Fatal("accepted invalid config")
	}
	after, _ := os.ReadFile(m.state)
	if !bytes.Equal(before, after) {
		t.Fatal("validation failure mutated state")
	}
}

func TestConcurrentAdditions(t *testing.T) {
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "trojan://secret@%s.test:443", strings.Trim(r.URL.Path, "/"))
	}))
	defer s.Close()
	m := fixtureManager(t, s.Client())
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("node%d", i)
			if w := postSubscription(m, Subscription{name, s.URL + "/" + name}); w.Code != 200 {
				t.Errorf("add = %d %s", w.Code, w.Body)
			}
		}(i)
	}
	wg.Wait()
	state, err := readState(m.state)
	if err != nil || len(state.Subscriptions) != 8 {
		t.Fatalf("lost additions: %+v %v", state, err)
	}
	if m.nodes != 8 {
		t.Fatalf("nodes = %d", m.nodes)
	}
}

func TestImplicitTLS(t *testing.T) {
	for _, scheme := range []string{"trojan", "hy2", "hysteria2"} {
		item, err := parseURI(scheme+"://secret@example.test:443?sni=tls.test&alpn=h2,h3&insecure=1", "node")
		if err != nil {
			t.Fatal(err)
		}
		tls := item["tls"].(map[string]any)
		if tls["enabled"] != true || tls["server_name"] != "tls.test" || tls["insecure"] != true || len(tls["alpn"].([]string)) != 2 {
			t.Fatalf("TLS = %#v", tls)
		}
	}
}

func TestRejectUnsupportedNodes(t *testing.T) {
	for _, uri := range []string{"vmess://encoded", "trojan://secret@host:0", "trojan://secret@host:65536", "vless://uuid@host:443?type=xhttp", "hy2://secret@host:443?type=ws", "ss://aes-128-gcm:secret@host:443?plugin=unknown", "vless://uuid@host:443?security=reality", "trojan://secret@host:443?security=none"} {
		if _, err := parseURI(uri, "node"); err == nil {
			t.Errorf("accepted %s", uri)
		}
	}
}

func TestBaseTagCollision(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "trojan://secret@host:443") }))
	defer s.Close()
	m := fixtureManager(t, s.Client())
	if err := writeJSONAtomic(m.base, map[string]any{"outbounds": []any{map[string]any{"type": "direct", "tag": "sub-fixture-node-1"}}}); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONAtomic(m.state, State{Subscriptions: []Subscription{{"fixture", s.URL}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.build(); err == nil {
		t.Fatal("accepted base collision")
	}
}

func TestRedirectPolicy(t *testing.T) {
	t.Setenv("REFRESH_INTERVAL", "1")
	m, err := newManager()
	if err != nil {
		t.Fatal(err)
	}
	for _, scheme := range []string{"http", "https"} {
		err := m.client.CheckRedirect(&http.Request{URL: &url.URL{Scheme: scheme}}, nil)
		if (err == nil) != (scheme == "https") {
			t.Fatalf("%s redirect: %v", scheme, err)
		}
	}
	if err := m.client.CheckRedirect(&http.Request{URL: &url.URL{Scheme: "https"}}, make([]*http.Request, 10)); err == nil {
		t.Fatal("redirect loop allowed")
	}
	var reached bool
	downstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	defer downstream.Close()
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, downstream.URL, http.StatusFound) }))
	defer s.Close()
	m.client.Transport = s.Client().Transport
	if _, err := m.fetch(s.URL); err == nil {
		t.Fatal("downgrade allowed")
	}
	if reached {
		t.Fatal("HTTP destination reached")
	}
}

func TestHealthAndInterval(t *testing.T) {
	for _, interval := range []string{"0", "-1", "invalid", "9223372036854775807"} {
		t.Setenv("REFRESH_INTERVAL", interval)
		if _, err := newManager(); err == nil {
			t.Fatalf("accepted interval %s", interval)
		}
	}
	t.Setenv("REFRESH_INTERVAL", "1")
	t.Setenv("STATE_FILE", "")
	production, err := newManager()
	if err != nil || production.state != "/data/state/subscriptions.json" {
		t.Fatalf("defaults = %+v, %v", production, err)
	}
	m := fixtureManager(t, http.DefaultClient)
	w := httptest.NewRecorder()
	m.handler(w, httptest.NewRequest(http.MethodGet, "/health", nil))
	if w.Code != 503 {
		t.Fatal("initial health ready")
	}
	if _, err := m.build(); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	m.handler(w, httptest.NewRequest(http.MethodGet, "/health", nil))
	if w.Code != 200 || m.lastSuccess.IsZero() || m.lastAttempt.IsZero() || m.nodes != 0 {
		t.Fatalf("health = %d %s", w.Code, w.Body)
	}
}

func TestBindFailure(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	m := fixtureManager(t, http.DefaultClient)
	m.port = listener.Addr().(*net.TCPAddr).Port
	m.interval = time.Second
	if err := m.run(); err == nil || err.Error() != "HTTP bind failed" {
		t.Fatalf("run = %v", err)
	}
}

func TestGeneratedTagCollision(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/one" {
			fmt.Fprint(w, "trojan://secret@one.test:443#b-c")
			return
		}
		fmt.Fprint(w, "trojan://secret@two.test:443#c")
	}))
	defer s.Close()
	m := fixtureManager(t, s.Client())
	if err := writeJSONAtomic(m.state, State{Subscriptions: []Subscription{{"a", s.URL + "/one"}, {"a-b", s.URL + "/two"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.build(); err == nil {
		t.Fatal("accepted colliding generated tags")
	}
}

func TestParseVLESSReality(t *testing.T) {
	item, err := parseURI("vless://uuid@example.test:443?security=reality&sni=ya.ru&pbk=public&sid=short&type=grpc&serviceName=proxy", "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if item["type"] != "vless" {
		t.Fatalf("type = %v", item["type"])
	}
	tls := item["tls"].(map[string]any)
	reality := tls["reality"].(map[string]any)
	if reality["public_key"] != "public" {
		t.Fatalf("reality = %#v", reality)
	}
	transport := item["transport"].(map[string]any)
	if transport["service_name"] != "proxy" {
		t.Fatalf("transport = %#v", transport)
	}
}

func TestBuildUsesLocalFixture(t *testing.T) {
	subscriptionServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("vless://uuid@example.test:443"))
	}))
	defer subscriptionServer.Close()
	directory := t.TempDir()
	basePath := filepath.Join(directory, "base.json")
	outputPath := filepath.Join(directory, "runtime", "config.json")
	statePath := filepath.Join(directory, "subscriptions.json")
	base := map[string]any{"outbounds": []any{
		map[string]any{"type": "urltest", "tag": "telegram-auto", "outbounds": []any{"direct-eth"}},
		map[string]any{"type": "direct", "tag": "direct-eth"},
	}}
	baseData, _ := json.Marshal(base)
	if err := os.WriteFile(basePath, baseData, 0o600); err != nil {
		t.Fatal(err)
	}
	state := State{Subscriptions: []Subscription{{Name: "fixture", URL: subscriptionServer.URL}}}
	stateData, _ := json.Marshal(state)
	if err := os.WriteFile(statePath, stateData, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Dir(outputPath), 0o755); err != nil {
		t.Fatal(err)
	}
	m := &Manager{base: basePath, output: outputPath, state: statePath, client: subscriptionServer.Client(), validate: func(string) error { return nil }}
	result, err := m.build()
	if err != nil {
		t.Fatal(err)
	}
	if result["nodes"] != 1 {
		t.Fatalf("result = %#v", result)
	}
	if _, err := os.Stat(outputPath); err != nil {
		t.Fatal(err)
	}
	generatedData, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	var generated map[string]any
	if err := json.Unmarshal(generatedData, &generated); err != nil {
		t.Fatal(err)
	}
	var telegramAuto map[string]any
	for _, outbound := range generated["outbounds"].([]any) {
		item := outbound.(map[string]any)
		if item["tag"] == "telegram-auto" {
			telegramAuto = item
		}
	}
	if got := telegramAuto["outbounds"].([]any); len(got) != 2 || got[1] != "direct-eth" {
		t.Fatalf("telegram fallback = %#v", telegramAuto["outbounds"])
	}
}

func TestWriteJSONAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := writeJSONAtomic(path, State{Subscriptions: []Subscription{{Name: "fixture", URL: "https://example.test"}}}); err != nil {
		t.Fatal(err)
	}
	state, err := readState(path)
	if err != nil || len(state.Subscriptions) != 1 {
		t.Fatalf("state = %#v, err = %v", state, err)
	}
}
