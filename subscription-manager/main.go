package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxRequestBody = 64 * 1024

type Subscription struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

type State struct {
	Subscriptions []Subscription `json:"subscriptions"`
}

type Manager struct {
	base     string
	output   string
	state    string
	interval time.Duration
	port     int
	client   *http.Client
	validate func(string) error
	mu       sync.Mutex
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func newManager() (*Manager, error) {
	interval, err := strconv.Atoi(env("REFRESH_INTERVAL", "3600"))
	if err != nil || interval < 0 {
		return nil, fmt.Errorf("invalid REFRESH_INTERVAL")
	}
	port, err := strconv.Atoi(env("PORT", "19091"))
	if err != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("invalid PORT")
	}
	m := &Manager{
		base:     env("BASE_CONFIG", "/data/base-config.json"),
		output:   env("OUTPUT_CONFIG", "/data/runtime/config.json"),
		state:    env("STATE_FILE", "/data/subscriptions.json"),
		interval: time.Duration(interval) * time.Second,
		port:     port,
		client:   &http.Client{Timeout: 30 * time.Second},
	}
	m.validate = validateConfig
	return m, nil
}

func readState(path string) (State, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return State{Subscriptions: []Subscription{}}, nil
	}
	if err != nil {
		return State{}, err
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return State{}, fmt.Errorf("decode state: %w", err)
	}
	return state, nil
}

func writeJSONAtomic(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryName, path)
}

func decodeBody(body []byte) []string {
	text := string(body)
	var found []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "vless://") || strings.HasPrefix(line, "vmess://") || strings.HasPrefix(line, "trojan://") || strings.HasPrefix(line, "ss://") || strings.HasPrefix(line, "hy2://") || strings.HasPrefix(line, "hysteria2://") {
			found = append(found, line)
		}
	}
	if len(found) > 0 {
		return found
	}
	compact := strings.Join(strings.Fields(text), "")
	decoded, err := decodeBase64(compact)
	if err != nil {
		return nil
	}
	var result []string
	for _, line := range strings.Split(string(decoded), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			result = append(result, line)
		}
	}
	return result
}

func decodeBase64(value string) ([]byte, error) {
	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		decoded, err := encoding.DecodeString(value)
		if err == nil {
			return decoded, nil
		}
	}
	return nil, errors.New("invalid base64")
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

func nodeName(raw string, index int) string {
	name := unsafeName.ReplaceAllString(raw, "-")
	name = strings.Trim(name, "-")
	if len(name) > 36 {
		name = name[:36]
	}
	if name == "" {
		name = "node"
	}
	return fmt.Sprintf("%s-%d", name, index)
}

func queryBool(query url.Values, names ...string) bool {
	for _, name := range names {
		if query.Get(name) == "1" || strings.EqualFold(query.Get(name), "true") {
			return true
		}
	}
	return false
}

func tlsFrom(query url.Values) map[string]any {
	security := query.Get("security")
	if security != "tls" && security != "reality" {
		return nil
	}
	result := map[string]any{"enabled": true}
	if sni := query.Get("sni"); sni != "" {
		result["server_name"] = sni
	}
	if queryBool(query, "insecure", "allowInsecure") {
		result["insecure"] = true
	}
	fingerprint := query.Get("fp")
	allowed := map[string]bool{"chrome": true, "firefox": true, "safari": true, "ios": true, "android": true, "edge": true}
	if !allowed[fingerprint] {
		fingerprint = ""
	}
	if fingerprint != "" || security == "reality" {
		if fingerprint == "" {
			fingerprint = "chrome"
		}
		result["utls"] = map[string]any{"enabled": true, "fingerprint": fingerprint}
	}
	if security == "reality" && query.Get("pbk") != "" {
		reality := map[string]any{"enabled": true, "public_key": query.Get("pbk")}
		if query.Get("sid") != "" {
			reality["short_id"] = query.Get("sid")
		}
		result["reality"] = reality
	}
	return result
}

func transportFrom(query url.Values) (map[string]any, error) {
	switch query.Get("type") {
	case "ws":
		path := query.Get("path")
		if strings.Contains(path, "%") {
			if _, err := url.PathUnescape(path); err != nil {
				return nil, errors.New("invalid websocket path escape")
			}
		}
		transport := map[string]any{"type": "ws"}
		if path != "" {
			transport["path"] = path
		}
		if host := query.Get("host"); host != "" {
			transport["headers"] = map[string]any{"Host": host}
		}
		return transport, nil
	case "grpc":
		transport := map[string]any{"type": "grpc"}
		if name := query.Get("serviceName"); name != "" {
			transport["service_name"] = name
		}
		return transport, nil
	default:
		return nil, nil
	}
}

func parseURI(raw, tag string) (map[string]any, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if parsed.Hostname() == "" || parsed.Port() == "" {
		return nil, errors.New("missing server or port")
	}
	query := parsed.Query()
	server := parsed.Hostname()
	port, _ := strconv.Atoi(parsed.Port())
	user := ""
	if parsed.User != nil {
		user = parsed.User.Username()
	}
	item := map[string]any{"tag": tag, "server": server, "server_port": port}
	switch strings.ToLower(parsed.Scheme) {
	case "vless":
		item["type"], item["uuid"] = "vless", user
		flow := query.Get("flow")
		if flow == "xtls-rprx-vision-udp443" {
			flow = "xtls-rprx-vision"
		}
		if flow == "xtls-rprx-vision" || flow == "xtls-rprx-direct" {
			item["flow"] = flow
		}
	case "trojan":
		item["type"], item["password"] = "trojan", user
	case "hy2", "hysteria2":
		item["type"], item["password"] = "hysteria2", user
		if query.Get("obfs") == "salamander" && query.Get("obfs-password") != "" {
			item["obfs"] = map[string]any{"type": "salamander", "password": query.Get("obfs-password")}
		}
	case "ss":
		userinfo := user
		if !strings.Contains(userinfo, ":") {
			decoded, err := decodeBase64(userinfo)
			if err != nil {
				return nil, errors.New("invalid shadowsocks credentials")
			}
			userinfo = string(decoded)
		}
		method, password, ok := strings.Cut(userinfo, ":")
		supported := map[string]bool{"aes-128-gcm": true, "aes-256-gcm": true, "chacha20-ietf-poly1305": true, "2022-blake3-aes-128-gcm": true, "2022-blake3-aes-256-gcm": true, "2022-blake3-chacha20-poly1305": true}
		if !ok || !supported[method] {
			return nil, fmt.Errorf("unsupported shadowsocks method: %s", method)
		}
		item["type"], item["method"], item["password"] = "shadowsocks", method, password
	default:
		return nil, errors.New("unsupported scheme")
	}
	if tls := tlsFrom(query); tls != nil {
		item["tls"] = tls
	} else if item["type"] == "trojan" || item["type"] == "hysteria2" {
		item["tls"] = map[string]any{"enabled": true}
	}
	if transport, err := transportFrom(query); err != nil {
		return nil, err
	} else if transport != nil {
		item["transport"] = transport
	}
	return item, nil
}

func (m *Manager) fetch(rawURL string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "subscription-manager/1.0")
	response, err := m.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("subscription returned HTTP %d", response.StatusCode)
	}
	return io.ReadAll(io.LimitReader(response.Body, 16*1024*1024))
}

func validateConfig(path string) error {
	command := exec.Command("sing-box", "check", "-c", path)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("sing-box check: %s", strings.TrimSpace(string(output)))
	}
	return nil
}

func (m *Manager) build() (map[string]any, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	baseData, err := os.ReadFile(m.base)
	if err != nil {
		return nil, err
	}
	var base map[string]any
	if err := json.Unmarshal(baseData, &base); err != nil {
		return nil, fmt.Errorf("decode base config: %w", err)
	}
	state, err := readState(m.state)
	if err != nil {
		return nil, err
	}
	var outbounds []map[string]any
	seen := map[string]bool{}
	errorsFound := make([]string, 0)
	for _, subscription := range state.Subscriptions {
		body, err := m.fetch(subscription.URL)
		if err != nil {
			errorsFound = append(errorsFound, fmt.Sprintf("%s: fetch failed: %v", subscription.Name, err))
			continue
		}
		for index, rawURI := range decodeBody(body) {
			tag := "sub-" + unsafeName.ReplaceAllString(subscription.Name, "-") + "-" + nodeName(fragment(rawURI), index+1)
			item, err := parseURI(rawURI, tag)
			if err != nil {
				errorsFound = append(errorsFound, fmt.Sprintf("%s:%d: %v", subscription.Name, index+1, err))
				continue
			}
			fingerprintData := make(map[string]any, len(item))
			for key, value := range item {
				if key != "tag" {
					fingerprintData[key] = value
				}
			}
			fingerprintBytes, _ := json.Marshal(fingerprintData)
			fingerprint := string(fingerprintBytes)
			if !seen[fingerprint] {
				seen[fingerprint] = true
				outbounds = append(outbounds, item)
			}
		}
	}
	generatedTags := make([]any, len(outbounds))
	for index, item := range outbounds {
		generatedTags[index] = item["tag"]
	}
	if values, ok := base["outbounds"].([]any); ok {
		for _, value := range values {
			if item, ok := value.(map[string]any); ok && (item["tag"] == "telegram-auto" || item["tag"] == "default-auto") {
				existing, _ := item["outbounds"].([]any)
				item["outbounds"] = append(generatedTags, existing...)
			}
		}
		filtered := make([]any, 0, len(values)+len(outbounds))
		for _, value := range values {
			item, _ := value.(map[string]any)
			if tag, _ := item["tag"].(string); !strings.HasPrefix(tag, "sub-") {
				filtered = append(filtered, value)
			}
		}
		for _, item := range outbounds {
			filtered = append(filtered, item)
		}
		base["outbounds"] = filtered
	}
	temporary, err := os.CreateTemp(filepath.Dir(m.output), ".config-*")
	if err != nil {
		return nil, err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	data, err := json.MarshalIndent(base, "", "  ")
	if err == nil {
		_, err = temporary.Write(append(data, '\n'))
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, err
	}
	if err := m.validate(temporaryName); err != nil {
		return nil, err
	}
	if existing, err := os.ReadFile(m.output); err == nil {
		_ = os.WriteFile(strings.TrimSuffix(m.output, filepath.Ext(m.output))+".previous.json", existing, 0o600)
	}
	if err := os.Rename(temporaryName, m.output); err != nil {
		return nil, err
	}
	return map[string]any{"nodes": len(outbounds), "errors": errorsFound[:min(20, len(errorsFound))]}, nil
}

func fragment(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	decoded, err := url.PathUnescape(parsed.Fragment)
	if err != nil {
		return parsed.Fragment
	}
	return decoded
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (m *Manager) reply(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (m *Manager) handler(w http.ResponseWriter, request *http.Request) {
	switch {
	case request.Method == http.MethodGet && request.URL.Path == "/health":
		m.reply(w, http.StatusOK, map[string]any{"ok": true, "output": m.output})
	case request.Method == http.MethodGet && request.URL.Path == "/subscriptions":
		state, err := readState(m.state)
		if err != nil {
			m.reply(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		names := make([]map[string]string, len(state.Subscriptions))
		for index, subscription := range state.Subscriptions {
			names[index] = map[string]string{"name": subscription.Name}
		}
		m.reply(w, http.StatusOK, map[string]any{"subscriptions": names})
	case request.Method == http.MethodPost && (request.URL.Path == "/subscriptions" || request.URL.Path == "/refresh"):
		if request.URL.Path == "/subscriptions" {
			if request.ContentLength <= 0 || request.ContentLength > maxRequestBody {
				m.reply(w, http.StatusBadRequest, map[string]string{"error": "request body must be between 1 and 65536 bytes"})
				return
			}
			var payload Subscription
			if err := json.NewDecoder(http.MaxBytesReader(w, request.Body, maxRequestBody)).Decode(&payload); err != nil {
				m.reply(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			parsed, err := url.Parse(payload.URL)
			if payload.Name == "" || err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" {
				m.reply(w, http.StatusBadRequest, map[string]string{"error": "name and an https URL are required"})
				return
			}
			state, err := readState(m.state)
			if err != nil {
				m.reply(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			filtered := state.Subscriptions[:0]
			for _, subscription := range state.Subscriptions {
				if subscription.Name != payload.Name {
					filtered = append(filtered, subscription)
				}
			}
			state.Subscriptions = append(filtered, payload)
			if err := writeJSONAtomic(m.state, state); err != nil {
				m.reply(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
		}
		result, err := m.build()
		if err != nil {
			m.reply(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		m.reply(w, http.StatusOK, result)
	default:
		m.reply(w, http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

func (m *Manager) run() error {
	if err := os.MkdirAll(filepath.Dir(m.output), 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(m.output); errors.Is(err, os.ErrNotExist) {
		data, err := os.ReadFile(m.base)
		if err != nil {
			return err
		}
		if err := os.WriteFile(m.output, data, 0o600); err != nil {
			return err
		}
	}
	server := &http.Server{Addr: fmt.Sprintf("127.0.0.1:%d", m.port), Handler: http.HandlerFunc(m.handler), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Printf("HTTP server failed: %v\n", err)
		}
	}()
	for {
		if _, err := m.build(); err != nil {
			fmt.Printf("refresh failed: %v\n", err)
		}
		time.Sleep(m.interval)
	}
}

func main() {
	m, err := newManager()
	if err != nil {
		fmt.Printf("configuration failed: %v\n", err)
		os.Exit(1)
	}
	if err := m.run(); err != nil {
		fmt.Printf("startup failed: %v\n", err)
		os.Exit(1)
	}
}
