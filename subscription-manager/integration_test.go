package main

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestIntegrationGeneratedConfig(t *testing.T) {
	image := os.Getenv("SING_BOX_TEST_IMAGE")
	if image == "" {
		t.Skip("set SING_BOX_TEST_IMAGE to validate generated configs with real sing-box in Docker")
	}

	// All credentials and endpoints are public synthetic fixtures, never live subscriptions.
	publicKey := base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	ssCredentials := base64.RawURLEncoding.EncodeToString([]byte("chacha20-ietf-poly1305:public-fixture-password"))
	fixtures := []struct {
		name string
		body string
	}{
		{
			name: "vless-tls-ws",
			body: "vless://550e8400-e29b-41d4-a716-446655440000@proxy.example.com:443?security=tls&sni=proxy.example.com&type=ws&path=%2Fpublic-fixture&host=proxy.example.com#fixture",
		},
		{
			name: "vless-reality-grpc",
			body: "vless://550e8400-e29b-41d4-a716-446655440000@proxy.example.com:443?security=reality&sni=proxy.example.com&type=grpc&serviceName=public-fixture&pbk=" + publicKey + "&sid=0123456789abcdef#fixture",
		},
		{
			name: "trojan-implicit-tls-sni",
			body: "trojan://public-fixture-password@proxy.example.com:443?sni=tls.example.com#fixture",
		},
		{
			name: "hy2-implicit-tls-sni",
			body: "hy2://public-fixture-password@proxy.example.com:443?sni=tls.example.com#fixture",
		},
		{
			name: "shadowsocks-modern-base64",
			body: "ss://" + ssCredentials + "@proxy.example.com:8388#fixture",
		},
	}
	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, fixture.body+"\n")
			}))
			defer provider.Close()

			dir := t.TempDir()
			m := &Manager{
				base:   filepath.Join(dir, "base.json"),
				output: filepath.Join(dir, "config.json"),
				state:  filepath.Join(dir, "state.json"),
				client: provider.Client(),
			}
			base := map[string]any{"outbounds": []map[string]any{
				{"type": "direct", "tag": "direct"},
				{"type": "urltest", "tag": "telegram-auto", "outbounds": []string{"direct"}, "url": "https://www.example.com/"},
				{"type": "urltest", "tag": "default-auto", "outbounds": []string{"direct"}, "url": "https://www.example.com/"},
			}}
			if err := writeJSONAtomic(m.base, base); err != nil {
				t.Fatal(err)
			}
			if err := writeJSONAtomic(m.state, State{Subscriptions: []Subscription{{Name: "fixture", URL: provider.URL}}}); err != nil {
				t.Fatal(err)
			}
			validated := false
			m.validate = func(path string) error {
				candidate, err := os.Open(path)
				if err != nil {
					return err
				}
				defer candidate.Close()
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer cancel()
				command := exec.CommandContext(ctx, "docker", "run", "--rm", "-i", image, "check", "-c", "/dev/stdin")
				command.Stdin = candidate
				output, err := command.CombinedOutput()
				if err != nil {
					// Safe here only because the candidate contains public test fixtures.
					t.Logf("sing-box Docker validation failed: %v\n%s", err, output)
					return err
				}
				validated = true
				return nil
			}
			result, err := m.build()
			if err != nil {
				t.Fatalf("build generated config: %v", err)
			}
			if !validated {
				t.Fatal("build did not validate the generated candidate")
			}
			if result["nodes"] != 1 {
				t.Fatalf("generated nodes = %v, want 1", result["nodes"])
			}
		})
	}
}
