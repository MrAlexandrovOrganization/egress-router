// Package nodeuri is a frozen copy of subscription-manager's URI parser.
// Keep production independent; review intentional parser changes in both places.
package nodeuri

import (
	"encoding/base64"
	"errors"
	"net/url"
	"strconv"
	"strings"
)

func decodeBase64(value string) ([]byte, error) {
	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		decoded, err := encoding.DecodeString(value)
		if err == nil {
			return decoded, nil
		}
	}
	return nil, errors.New("invalid base64")
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

func ParseURI(raw, tag string) (map[string]any, error) {
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
