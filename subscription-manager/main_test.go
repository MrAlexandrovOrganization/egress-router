package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestDecodeBody(t *testing.T) {
	plain := []byte("vless://uuid@example.test:443\ninvalid\n")
	got := decodeBody(plain)
	if len(got) != 1 || got[0] != "vless://uuid@example.test:443" {
		t.Fatalf("decode plain = %#v", got)
	}
	encoded := base64.StdEncoding.EncodeToString([]byte("vless://uuid@example.test:443\n"))
	got = decodeBody([]byte(encoded))
	if len(got) != 1 || got[0] != "vless://uuid@example.test:443" {
		t.Fatalf("decode base64 = %#v", got)
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
		map[string]any{"type": "urltest", "tag": "telegram-auto", "outbounds": []any{}},
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
