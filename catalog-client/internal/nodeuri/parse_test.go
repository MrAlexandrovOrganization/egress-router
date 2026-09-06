package nodeuri

import (
	"encoding/base64"
	"reflect"
	"strings"
	"testing"
)

func TestParserFixtures(t *testing.T) {
	fixtures := []struct {
		uri  string
		want map[string]any
	}{
		{"vless://fixture@proxy.example:443?security=tls&sni=tls.example&type=ws&path=%2Fsocket&host=ws.example&alpn=h2,http%2F1.1&fp=firefox&allowInsecure=true&flow=xtls-rprx-vision-udp443", map[string]any{
			"type": "vless", "uuid": "fixture", "flow": "xtls-rprx-vision", "tls": map[string]any{"enabled": true, "server_name": "tls.example", "alpn": []string{"h2", "http/1.1"}, "insecure": true, "utls": map[string]any{"enabled": true, "fingerprint": "firefox"}}, "transport": map[string]any{"type": "ws", "path": "/socket", "headers": map[string]any{"Host": "ws.example"}},
		}},
		{"vless://fixture@proxy.example:443?security=reality&pbk=public&sid=abcd&type=grpc&serviceName=svc&fp=unknown", map[string]any{
			"type": "vless", "uuid": "fixture", "tls": map[string]any{"enabled": true, "utls": map[string]any{"enabled": true, "fingerprint": "chrome"}, "reality": map[string]any{"enabled": true, "public_key": "public", "short_id": "abcd"}}, "transport": map[string]any{"type": "grpc", "service_name": "svc"},
		}},
		{"trojan://fixture:password@proxy.example:443?sni=tls.example", map[string]any{"type": "trojan", "password": "fixture:password", "tls": map[string]any{"enabled": true, "server_name": "tls.example"}}},
		{"hy2://fixture:password@proxy.example:443?obfs=salamander&obfs-password=public", map[string]any{"type": "hysteria2", "password": "fixture:password", "obfs": map[string]any{"type": "salamander", "password": "public"}, "tls": map[string]any{"enabled": true}}},
		{"ss://" + base64.RawURLEncoding.EncodeToString([]byte("aes-128-gcm:fixture")) + "@proxy.example:443", map[string]any{"type": "shadowsocks", "method": "aes-128-gcm", "password": "fixture"}},
		{"ss://aes-256-gcm:fixture@proxy.example:443", map[string]any{"type": "shadowsocks", "method": "aes-256-gcm", "password": "fixture"}},
	}
	for i, fixture := range fixtures {
		fixture.want["tag"], fixture.want["server"], fixture.want["server_port"] = "node", "proxy.example", 443
		got, err := ParseURI(fixture.uri, "node")
		if err != nil || !reflect.DeepEqual(got, fixture.want) {
			t.Fatalf("fixture %d mismatch: %v / %v", i, got, err)
		}
	}
}

func TestRejectAndSanitize(t *testing.T) {
	for _, uri := range []string{
		"%synthetic-secret", "vmess://synthetic-secret", "trojan://synthetic-secret@host", "trojan://synthetic-secret@host:65536", "vless://synthetic-secret@host:443?bad=%xx", "vless://synthetic-secret@host:443?security=bad", "vless://synthetic-secret@host:443?security=reality", "vless://synthetic-secret@host:443?plugin=x", "vless://host:443", "trojan://synthetic-secret@host:443?security=none", "hy2://synthetic-secret@host:443?obfs=bad", "hy2://synthetic-secret@host:443?type=ws", "ss://synthetic-secret@host:443", "ss://bad:synthetic-secret@host:443", "vless://synthetic-secret@host:443?type=kcp", "vless://synthetic-secret@host:443?type=ws&path=%25xx",
	} {
		_, err := ParseURI(uri, "node")
		if err == nil || strings.Contains(err.Error(), "synthetic-secret") {
			t.Fatal("missing or unsafe parser error")
		}
	}
}
