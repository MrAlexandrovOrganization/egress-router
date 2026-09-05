#!/usr/bin/env python3
"""Fetch v2ray-style subscriptions and build a sing-box config."""

import base64
import json
import os
import re
import subprocess
import tempfile
import threading
import time
from typing import Any
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import parse_qs, unquote, urlsplit
from urllib.request import Request, urlopen

BASE = Path(os.environ.get("BASE_CONFIG", "/data/base-config.json"))
OUTPUT = Path(os.environ.get("OUTPUT_CONFIG", "/data/runtime/config.json"))
STATE = Path(os.environ.get("STATE_FILE", "/data/subscriptions.json"))
INTERVAL = int(os.environ.get("REFRESH_INTERVAL", "3600"))
PORT = int(os.environ.get("PORT", "19091"))
LOCK = threading.Lock()
MAX_REQUEST_BODY = 64 * 1024


def read_state():
    if not STATE.exists():
        return {"subscriptions": []}
    return json.loads(STATE.read_text())


def write_state(state):
    STATE.parent.mkdir(parents=True, exist_ok=True)
    fd, temporary = tempfile.mkstemp(prefix="subscriptions.", suffix=".json", dir=STATE.parent)
    os.close(fd)
    try:
        Path(temporary).write_text(json.dumps(state, indent=2) + "\n")
        os.replace(temporary, STATE)
    finally:
        Path(temporary).unlink(missing_ok=True)


def decode_body(body):
    text = body.decode("utf-8", "replace")
    lines = [line.strip() for line in text.splitlines()]
    found = [line for line in lines if line.startswith(("vless://", "vmess://", "trojan://", "ss://", "hy2://", "hysteria2://"))]
    if found:
        return found
    compact = re.sub(r"\s+", "", text)
    try:
        decoded = base64.b64decode(compact + "===", validate=False).decode("utf-8", "replace")
    except Exception:
        return []
    return [line.strip() for line in decoded.splitlines() if line.strip()]


def name_for(uri, index):
    label = unquote(urlsplit(uri).fragment).strip()
    label = re.sub(r"[^A-Za-z0-9_-]+", "-", label).strip("-")
    return (label[:36] or "node") + "-" + str(index)


def tls_from(query):
    security = query.get("security", [""])[0]
    if security not in ("tls", "reality"):
        return None
    result: dict[str, Any] = {"enabled": True}
    sni = query.get("sni", [""])[0]
    if sni:
        result["server_name"] = sni
    if query.get("insecure", ["0"])[0] in ("1", "true") or query.get("allowInsecure", ["0"])[0] in ("1", "true"):
        result["insecure"] = True
    fp = query.get("fp", [""])[0]
    if fp not in {"chrome", "firefox", "safari", "ios", "android", "edge"}:
        fp = ""
    if fp or security == "reality":
        result["utls"] = {"enabled": True, "fingerprint": fp or "chrome"}
    if security == "reality" and query.get("pbk", [""])[0]:
        result["reality"] = {"enabled": True, "public_key": query["pbk"][0]}
        if query.get("sid", [""])[0]:
            result["reality"]["short_id"] = query["sid"][0]
    return result


def transport_from(query):
    kind = query.get("type", [""])[0]
    if kind == "ws":
        transport: dict[str, Any] = {"type": "ws"}
        path = query.get("path", [""])[0]
        if re.search(r"%(?![0-9A-Fa-f]{2})", path):
            raise ValueError("invalid websocket path escape")
        if path:
            transport["path"] = path
        if query.get("host", [""])[0]:
            transport["headers"] = {"Host": query["host"][0]}
        return transport
    if kind == "grpc":
        transport = {"type": "grpc"}
        if query.get("serviceName", [""])[0]:
            transport["service_name"] = query["serviceName"][0]
        return transport
    return None


def parse_uri(uri, tag):
    parsed = urlsplit(uri)
    scheme = parsed.scheme.lower()
    query = parse_qs(parsed.query)
    host, port = parsed.hostname, parsed.port
    if not host or not port:
        raise ValueError("missing server or port")
    if scheme == "vless":
        item = {"type": "vless", "tag": tag, "server": host, "server_port": port, "uuid": unquote(parsed.username or "")}
        flow = query.get("flow", [""])[0]
        if flow == "xtls-rprx-vision-udp443":
            flow = "xtls-rprx-vision"
        if flow in {"xtls-rprx-vision", "xtls-rprx-direct", "xtls-rprx-vision-udp443"}:
            item["flow"] = flow
        tls = tls_from(query)
        if tls:
            item["tls"] = tls
        transport = transport_from(query)
        if transport:
            item["transport"] = transport
        return item
    if scheme == "trojan":
        item = {"type": "trojan", "tag": tag, "server": host, "server_port": port, "password": unquote(parsed.username or "")}
        item["tls"] = tls_from(query) or {"enabled": True}
        transport = transport_from(query)
        if transport:
            item["transport"] = transport
        return item
    if scheme in ("hy2", "hysteria2"):
        item = {"type": "hysteria2", "tag": tag, "server": host, "server_port": port, "password": unquote(parsed.username or "")}
        item["tls"] = tls_from(query) or {"enabled": True}
        if query.get("obfs", [""])[0] == "salamander" and query.get("obfs-password", [""])[0]:
            item["obfs"] = {"type": "salamander", "password": query["obfs-password"][0]}
        return item
    if scheme == "ss":
        userinfo = unquote(parsed.netloc.rsplit("@", 1)[0])
        if ":" not in userinfo:
            try:
                userinfo = base64.urlsafe_b64decode(userinfo + "===").decode()
            except Exception:
                raise ValueError("invalid shadowsocks credentials")
        method, password = userinfo.split(":", 1)
        supported = {"aes-128-gcm", "aes-256-gcm", "chacha20-ietf-poly1305", "2022-blake3-aes-128-gcm", "2022-blake3-aes-256-gcm", "2022-blake3-chacha20-poly1305"}
        if method not in supported:
            raise ValueError(f"unsupported shadowsocks method: {method}")
        return {"type": "shadowsocks", "tag": tag, "server": host, "server_port": port, "method": method, "password": password}
    raise ValueError("unsupported scheme")


def fetch(url):
    request = Request(url, headers={"User-Agent": "subscription-manager/1.0"})
    with urlopen(request, timeout=30) as response:
        return response.read()


def build():
    with LOCK:
        base = json.loads(BASE.read_text())
        state = read_state()
        outbounds = []
        errors = []
        seen = set()
        for subscription in state.get("subscriptions", []):
            try:
                lines = decode_body(fetch(subscription["url"]))
            except Exception as exc:
                errors.append(f"{subscription['name']}: fetch failed: {exc}")
                continue
            for index, uri in enumerate(lines, 1):
                try:
                    tag = "sub-" + re.sub(r"[^A-Za-z0-9_-]+", "-", subscription["name"]).strip("-") + "-" + name_for(uri, index)
                    item = parse_uri(uri, tag)
                    fingerprint = json.dumps({k: v for k, v in item.items() if k != "tag"}, sort_keys=True)
                    if fingerprint not in seen:
                        seen.add(fingerprint)
                        outbounds.append(item)
                except Exception as exc:
                    errors.append(f"{subscription['name']}:{index}: {exc}")
        generated_tags = [item["tag"] for item in outbounds]
        for item in base.get("outbounds", []):
            if item.get("tag") in ("telegram-auto", "default-auto"):
                existing = item.get("outbounds", [])
                item["outbounds"] = generated_tags + [tag for tag in existing if tag not in generated_tags]
        base["outbounds"] = [item for item in base.get("outbounds", []) if not item.get("tag", "").startswith("sub-")] + outbounds
        OUTPUT.parent.mkdir(parents=True, exist_ok=True)
        fd, temporary = tempfile.mkstemp(prefix="config.", suffix=".json", dir=OUTPUT.parent)
        os.close(fd)
        Path(temporary).write_text(json.dumps(base, indent=2) + "\n")
        os.chmod(temporary, 0o644)
        check = subprocess.run(["sing-box", "check", "-c", temporary], capture_output=True, text=True)
        if check.returncode:
            Path(temporary).unlink(missing_ok=True)
            raise RuntimeError(check.stderr.strip() or "sing-box config check failed")
        if OUTPUT.exists():
            OUTPUT.with_suffix(".previous.json").write_bytes(OUTPUT.read_bytes())
        os.replace(temporary, OUTPUT)
        return {"nodes": len(outbounds), "errors": errors[:20]}


class Handler(BaseHTTPRequestHandler):
    def log_message(self, format, *args):
        return

    def reply(self, status, value):
        body = json.dumps(value).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path == "/health":
            return self.reply(200, {"ok": True, "output": str(OUTPUT)})
        if self.path == "/subscriptions":
            return self.reply(200, {"subscriptions": [{"name": x["name"]} for x in read_state().get("subscriptions", [])]})
        self.reply(404, {"error": "not found"})

    def do_POST(self):
        if self.path not in ("/subscriptions", "/refresh"):
            return self.reply(404, {"error": "not found"})
        try:
            if self.path == "/subscriptions":
                length = int(self.headers.get("Content-Length", "0"))
                if length <= 0 or length > MAX_REQUEST_BODY:
                    raise ValueError("request body must be between 1 and 65536 bytes")
                payload = json.loads(self.rfile.read(length))
                url = payload.get("url", "")
                parsed_url = urlsplit(url)
                if not payload.get("name") or parsed_url.scheme != "https" or not parsed_url.hostname:
                    raise ValueError("name and an https URL are required")
                state = read_state()
                state["subscriptions"] = [x for x in state["subscriptions"] if x["name"] != payload["name"]]
                state["subscriptions"].append({"name": payload["name"], "url": url})
                write_state(state)
            return self.reply(200, build())
        except Exception as exc:
            self.reply(400, {"error": str(exc)})


def main():
    OUTPUT.parent.mkdir(parents=True, exist_ok=True)
    if not OUTPUT.exists():
        OUTPUT.write_bytes(BASE.read_bytes())
    server = ThreadingHTTPServer(("127.0.0.1", PORT), Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    while True:
        try:
            build()
        except Exception as exc:
            print(f"refresh failed: {exc}", flush=True)
        time.sleep(INTERVAL)


if __name__ == "__main__":
    main()
