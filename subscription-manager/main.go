package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
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
	base        string
	output      string
	state       string
	interval    time.Duration
	port        int
	client      *http.Client
	validate    func(string) error
	mu          sync.Mutex
	healthMu    sync.Mutex
	lastAttempt time.Time
	lastSuccess time.Time
	nodes       int
	lastError   string
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func newManager() (*Manager, error) {
	interval, err := strconv.Atoi(env("REFRESH_INTERVAL", "3600"))
	if err != nil || interval <= 0 || int64(interval) > int64((1<<63-1)/time.Second) {
		return nil, fmt.Errorf("invalid REFRESH_INTERVAL")
	}
	port, err := strconv.Atoi(env("PORT", "19091"))
	if err != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("invalid PORT")
	}
	m := &Manager{
		base:     env("BASE_CONFIG", "/data/base-config.json"),
		output:   env("OUTPUT_CONFIG", "/data/runtime/config.json"),
		state:    env("STATE_FILE", "/data/state/subscriptions.json"),
		interval: time.Duration(interval) * time.Second,
		port:     port,
		client:   &http.Client{Timeout: 30 * time.Second, CheckRedirect: secureRedirect},
	}
	m.validate = validateConfig
	return m, nil
}

func secureRedirect(req *http.Request, via []*http.Request) error {
	if req.URL.Scheme != "https" {
		return errors.New("redirect requires HTTPS")
	}
	if len(via) >= 10 {
		return errors.New("too many redirects")
	}
	return nil
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
		if line != "" {
			found = append(found, line)
		}
	}
	if strings.Contains(text, "://") {
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
	if alpn := query.Get("alpn"); alpn != "" {
		result["alpn"] = strings.Split(alpn, ",")
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
	case "", "tcp":
		return nil, nil
	default:
		return nil, errors.New("unsupported transport")
	}
}

func parseURI(raw, tag string) (map[string]any, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("invalid node URI")
	}
	switch strings.ToLower(parsed.Scheme) {
	case "vless", "trojan", "hy2", "hysteria2", "ss":
	default:
		return nil, errors.New("unsupported node format (including VMess, Clash and sing-box JSON)")
	}
	if parsed.Hostname() == "" || parsed.Port() == "" {
		return nil, errors.New("missing server or port")
	}
	query, err := url.ParseQuery(parsed.RawQuery)
	if err != nil {
		return nil, errors.New("invalid node query")
	}
	if security := query.Get("security"); security != "" && security != "none" && security != "tls" && security != "reality" {
		return nil, errors.New("unsupported TLS security")
	}
	if query.Get("security") == "reality" && query.Get("pbk") == "" {
		return nil, errors.New("reality requires public key")
	}
	if query.Get("plugin") != "" || query.Get("network") != "" {
		return nil, errors.New("unsupported plugin or network option")
	}
	server := parsed.Hostname()
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port < 1 || port > 65535 {
		return nil, errors.New("invalid server port")
	}
	user := ""
	if parsed.User != nil {
		user = parsed.User.Username()
	}
	if user == "" {
		return nil, errors.New("missing node credentials")
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
		if password, ok := parsed.User.Password(); ok {
			item["password"] = user + ":" + password
		}
	case "hy2", "hysteria2":
		item["type"], item["password"] = "hysteria2", user
		if password, ok := parsed.User.Password(); ok {
			item["password"] = user + ":" + password
		}
		if query.Get("obfs") != "" && (query.Get("obfs") != "salamander" || query.Get("obfs-password") == "") {
			return nil, errors.New("unsupported or incomplete hysteria2 obfuscation")
		}
		if query.Get("obfs") == "salamander" && query.Get("obfs-password") != "" {
			item["obfs"] = map[string]any{"type": "salamander", "password": query.Get("obfs-password")}
		}
	case "ss":
		userinfo := user
		if password, ok := parsed.User.Password(); ok {
			userinfo = user + ":" + password
		}
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
			return nil, errors.New("unsupported shadowsocks method")
		}
		item["type"], item["method"], item["password"] = "shadowsocks", method, password
	default:
		return nil, errors.New("unsupported scheme")
	}
	if item["type"] == "trojan" || item["type"] == "hysteria2" {
		if query.Get("security") == "" {
			query.Set("security", "tls")
		}
		if query.Get("security") != "tls" {
			return nil, errors.New("protocol requires TLS")
		}
	}
	if tls := tlsFrom(query); tls != nil {
		item["tls"] = tls
	}
	if transport, err := transportFrom(query); err != nil {
		return nil, err
	} else if transport != nil {
		if item["type"] == "hysteria2" || item["type"] == "shadowsocks" {
			return nil, errors.New("transport unsupported for protocol")
		}
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
	const limit = 16 * 1024 * 1024
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if len(body) > limit {
		return nil, errors.New("subscription too large")
	}
	return body, err
}

func validateConfig(path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "sing-box", "check", "-c", path)
	if _, err := command.CombinedOutput(); err != nil {
		return errors.New("sing-box config validation failed")
	}
	return nil
}

func (m *Manager) build() (map[string]any, error) {
	return m.update(nil)
}

// Hold the transaction lock through read, prospective validation and publication.
func (m *Manager) update(add *Subscription) (result map[string]any, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.healthMu.Lock()
	m.lastAttempt = time.Now().UTC()
	m.healthMu.Unlock()
	defer func() {
		m.healthMu.Lock()
		defer m.healthMu.Unlock()
		if err != nil {
			// Only fixed stage messages and parser errors may leave this boundary.
			m.lastError = err.Error()
			log.Printf("refresh failed: %s", m.lastError)
		} else {
			m.lastError = ""
			m.lastSuccess = time.Now().UTC()
			m.nodes = result["nodes"].(int)
		}
	}()
	state, err := readState(m.state)
	if err != nil {
		return nil, errors.New("read state failed")
	}
	previous := State{Subscriptions: append([]Subscription(nil), state.Subscriptions...)}
	if add != nil {
		filtered := make([]Subscription, 0, len(state.Subscriptions)+1)
		for _, sub := range state.Subscriptions {
			if sub.Name != add.Name {
				filtered = append(filtered, sub)
			}
		}
		state.Subscriptions = append(filtered, *add)
	}
	return m.buildState(state, previous, add != nil)
}

func (m *Manager) buildState(state, previous State, persist bool) (map[string]any, error) {
	baseData, err := os.ReadFile(m.base)
	if err != nil {
		return nil, errors.New("read base config failed")
	}
	var base map[string]any
	if err := json.Unmarshal(baseData, &base); err != nil {
		return nil, errors.New("decode base config failed")
	}
	values, ok := base["outbounds"].([]any)
	if !ok {
		return nil, errors.New("base outbounds must be an array")
	}
	tags := map[string]bool{}
	for _, value := range values {
		item, ok := value.(map[string]any)
		if !ok {
			return nil, errors.New("invalid base outbound")
		}
		tag, _ := item["tag"].(string)
		if tag != "" && tags[tag] {
			return nil, errors.New("base outbound tag collision")
		}
		tags[tag] = true
	}
	var outbounds []map[string]any
	seen := map[string]bool{}
	names := map[string]bool{}
	for _, subscription := range state.Subscriptions {
		name := unsafeName.ReplaceAllString(subscription.Name, "-")
		if subscription.Name == "" || names[name] {
			return nil, errors.New("normalized subscription name collision or empty name")
		}
		names[name] = true
	}
	for providerIndex, subscription := range state.Subscriptions {
		body, err := m.fetch(subscription.URL)
		if err != nil {
			return nil, fmt.Errorf("provider %d: fetch failed", providerIndex+1)
		}
		uris := decodeBody(body)
		if len(uris) == 0 {
			return nil, fmt.Errorf("provider %d: empty or unsupported subscription format", providerIndex+1)
		}
		for index, rawURI := range uris {
			tag := "sub-" + unsafeName.ReplaceAllString(subscription.Name, "-") + "-" + nodeName(fragment(rawURI), index+1)
			item, err := parseURI(rawURI, tag)
			if err != nil {
				return nil, fmt.Errorf("provider %d node %d: %s", providerIndex+1, index+1, err)
			}
			if tags[tag] {
				return nil, errors.New("outbound tag collision")
			}
			tags[tag] = true
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
		filtered := append([]any(nil), values...)
		for _, item := range outbounds {
			filtered = append(filtered, item)
		}
		base["outbounds"] = filtered
	}
	if err := os.MkdirAll(filepath.Dir(m.output), 0o755); err != nil {
		return nil, errors.New("create output directory failed")
	}
	temporary, err := os.CreateTemp(filepath.Dir(m.output), ".config-*")
	if err != nil {
		return nil, errors.New("create candidate config failed")
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o640); err != nil {
		_ = temporary.Close()
		return nil, errors.New("set candidate permissions failed")
	}
	data, err := json.MarshalIndent(base, "", "  ")
	if err == nil {
		_, err = temporary.Write(append(data, '\n'))
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, errors.New("write candidate config failed")
	}
	if err := m.validate(temporaryName); err != nil {
		return nil, errors.New("config validation failed")
	}
	if persist {
		// Both files are validated before commit. A process crash between these
		// renames is not atomic across files; the next refresh rebuilds from state.
		if err := writeJSONAtomic(m.state, state); err != nil {
			return nil, errors.New("persist state failed")
		}
	}
	if err := os.Rename(temporaryName, m.output); err != nil {
		if persist {
			if rollbackErr := writeJSONAtomic(m.state, previous); rollbackErr != nil {
				return nil, errors.New("publish config and rollback state failed")
			}
		}
		return nil, errors.New("publish config failed")
	}
	return map[string]any{"nodes": len(outbounds), "errors": []string{}}, nil
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

func (m *Manager) reply(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (m *Manager) handler(w http.ResponseWriter, request *http.Request) {
	switch {
	case request.Method == http.MethodGet && request.URL.Path == "/health":
		m.healthMu.Lock()
		defer m.healthMu.Unlock()
		ready := !m.lastSuccess.IsZero() && m.lastError == ""
		status := http.StatusOK
		if !ready {
			status = http.StatusServiceUnavailable
		}
		m.reply(w, status, map[string]any{"ok": ready, "last_attempt": m.lastAttempt, "last_success": m.lastSuccess, "nodes": m.nodes, "error": m.lastError})
	case request.Method == http.MethodGet && request.URL.Path == "/subscriptions":
		m.mu.Lock()
		state, err := readState(m.state)
		m.mu.Unlock()
		if err != nil {
			m.reply(w, http.StatusInternalServerError, map[string]string{"error": "read state failed"})
			return
		}
		names := make([]map[string]string, len(state.Subscriptions))
		for index, subscription := range state.Subscriptions {
			names[index] = map[string]string{"name": subscription.Name}
		}
		m.reply(w, http.StatusOK, map[string]any{"subscriptions": names})
	case request.Method == http.MethodPost && (request.URL.Path == "/subscriptions" || request.URL.Path == "/refresh"):
		var add *Subscription
		if request.URL.Path == "/subscriptions" {
			if request.ContentLength <= 0 || request.ContentLength > maxRequestBody {
				m.reply(w, http.StatusBadRequest, map[string]string{"error": "request body must be between 1 and 65536 bytes"})
				return
			}
			var payload Subscription
			decoder := json.NewDecoder(http.MaxBytesReader(w, request.Body, maxRequestBody))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&payload); err != nil {
				m.reply(w, http.StatusBadRequest, map[string]string{"error": "invalid request JSON"})
				return
			}
			if err := decoder.Decode(new(any)); err != io.EOF {
				m.reply(w, http.StatusBadRequest, map[string]string{"error": "request must contain one JSON object"})
				return
			}
			parsed, err := url.Parse(payload.URL)
			if payload.Name == "" || err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" {
				m.reply(w, http.StatusBadRequest, map[string]string{"error": "name and an https URL are required"})
				return
			}
			add = &payload
		}
		result, err := m.update(add)
		if err != nil {
			m.reply(w, http.StatusBadRequest, map[string]string{"error": "refresh failed; check service logs"})
			return
		}
		m.reply(w, http.StatusOK, result)
	default:
		m.reply(w, http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

func (m *Manager) run() error {
	if m.interval <= 0 {
		return errors.New("refresh interval must be positive")
	}
	server := &http.Server{Addr: fmt.Sprintf("127.0.0.1:%d", m.port), Handler: http.HandlerFunc(m.handler), ReadHeaderTimeout: 5 * time.Second}
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return errors.New("HTTP bind failed")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	defer func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if server.Shutdown(shutdown) != nil {
			_ = server.Close()
		}
	}()
	serverErrors := make(chan error, 1)
	go func() {
		serverErrors <- server.Serve(listener)
	}()
	refreshDone := make(chan struct{}, 1)
	start := func() { go func() { _, _ = m.build(); refreshDone <- struct{}{} }() }
	start()
	timer := time.NewTimer(m.interval)
	defer timer.Stop()
	timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-serverErrors:
			return errors.New("HTTP server stopped")
		case <-refreshDone:
			timer.Reset(m.interval)
		case <-timer.C:
			start()
		}
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
