// Package shadow builds candidates only. It has no activation or mutation RPCs.
package shadow

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	catalogv1 "github.com/MrAlexandrovOrganization/egress-router/catalog-client/internal/gen/catalogv1"
	"github.com/MrAlexandrovOrganization/egress-router/catalog-client/internal/nodeuri"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

const Limit = 50

// Dial accepts host:port only, avoiding resolver/URI ambiguity at the trust boundary.
// Non-loopback plaintext requires both explicit flags and an operator-trusted network.
func Dial(addr, token string, allowInsecure, trustedNetwork bool) (*grpc.ClientConn, error) {
	host, port, err := net.SplitHostPort(addr)
	p, portErr := strconv.Atoi(port)
	if err != nil || host == "" || portErr != nil || p < 1 || p > 65535 || strings.ContainsAny(host, "/% \t\r\n") {
		return nil, errors.New("invalid catalog address")
	}
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		return nil, errors.New("invalid catalog token")
	}
	var transport credentials.TransportCredentials = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
	if allowInsecure {
		ip := net.ParseIP(host)
		if host != "localhost" && (ip == nil || !ip.IsLoopback()) && !trustedNetwork {
			return nil, errors.New("plaintext requires loopback or explicit trusted network")
		}
		transport = insecure.NewCredentials()
	} else if trustedNetwork {
		return nil, errors.New("trusted network requires allow-insecure")
	}
	conn, err := grpc.NewClient("passthrough:///"+addr, grpc.WithTransportCredentials(transport), grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(4*1024*1024)))
	if err != nil {
		return nil, errors.New("catalog client setup failed")
	}
	return conn, nil
}

// Token deliberately does not accept tokens on the command line.
func Token(value, file string) (string, error) {
	if value != "" && file != "" {
		return "", errors.New("set only one catalog token source")
	}
	if file != "" {
		f, err := os.Open(file)
		if err != nil {
			return "", errors.New("read catalog token failed")
		}
		defer f.Close()
		buffer, err := io.ReadAll(io.LimitReader(f, 8193))
		if err != nil || len(buffer) > 8192 {
			return "", errors.New("read catalog token failed")
		}
		value = strings.TrimSpace(string(buffer))
	}
	if value == "" || len(value) > 8192 {
		return "", errors.New("catalog token required")
	}
	for _, c := range value {
		if c < 33 || c > 126 {
			return "", errors.New("invalid catalog token")
		}
	}
	return value, nil
}

type Options struct {
	Base, Output, Token string
	MaxAge              time.Duration
}

type Result struct {
	Version string `json:"version"`
	Count   int    `json:"count"`
}

func validSnapshot(s *catalogv1.Snapshot, maxAge time.Duration, now time.Time) bool {
	if maxAge < time.Second || s == nil || s.Id == "" || s.Profile != "server" || len(s.Nodes) == 0 || len(s.Nodes) > Limit {
		return false
	}
	n := now.Unix()
	age := int64(maxAge / time.Second)
	if s.CreatedAtUnix <= 0 || s.CreatedAtUnix > n || s.CreatedAtUnix < n-age || s.ValidUntilUnix <= n || s.ValidUntilUnix <= s.CreatedAtUnix {
		return false
	}
	for _, node := range s.Nodes {
		if node == nil || node.SchemaVersion != 1 || node.Uri == "" || node.LastSuccessUnix <= 0 || node.LastSuccessUnix > n || node.LastSuccessUnix < n-age {
			return false
		}
	}
	return true
}

// Only this dedicated filename is publishable, in a pre-existing non-symlink
// directory. The directory must be operator-owned, not concurrently writable by
// untrusted users. Refuse aliases instead of resolving them into live locations.
func CheckOutput(base, output string) error {
	fail := errors.New("unsafe shadow output path")
	if base == "" || output == "" || filepath.Base(output) != "catalog-shadow.json" {
		return fail
	}
	abs, err := filepath.Abs(output)
	if err != nil {
		return fail
	}
	baseAbs, err := filepath.Abs(base)
	if err != nil || abs == baseAbs {
		return fail
	}
	for path := abs; ; path = filepath.Dir(path) {
		info, err := os.Lstat(path)
		if err != nil && !(path == abs && errors.Is(err, os.ErrNotExist)) {
			return fail
		}
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return fail
			}
			if path == abs && !info.Mode().IsRegular() {
				return fail
			}
			if path != abs && !info.IsDir() {
				return fail
			}
		}
		if path == filepath.Dir(path) {
			break
		}
	}
	if outInfo, err := os.Stat(abs); err == nil {
		if baseInfo, err := os.Stat(base); err == nil && os.SameFile(outInfo, baseInfo) {
			return fail
		}
	}
	return nil
}

func Validate(ctx context.Context, executable, path string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	// Never capture or forward validator diagnostics: they may contain credentials.
	if err := exec.CommandContext(ctx, executable, "check", "-c", path).Run(); err != nil {
		return errors.New("sing-box validation failed")
	}
	return nil
}

func Run(ctx context.Context, client catalogv1.CatalogServiceClient, opts Options, validate func(context.Context, string) error) (Result, error) {
	if err := CheckOutput(opts.Base, opts.Output); err != nil {
		return Result{}, err
	}
	if opts.MaxAge < time.Second || validate == nil {
		return Result{}, errors.New("invalid shadow options")
	}
	rpcCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	rpcCtx = metadata.AppendToOutgoingContext(rpcCtx, "authorization", "Bearer "+opts.Token)
	snapshot, err := client.GetSnapshot(rpcCtx, &catalogv1.GetSnapshotRequest{Profile: "server", Limit: Limit, MaxAgeSeconds: int64(opts.MaxAge / time.Second)})
	if err != nil {
		return Result{}, errors.New("catalog snapshot request failed")
	}
	if !validSnapshot(snapshot, opts.MaxAge, time.Now()) {
		return Result{}, errors.New("invalid or stale catalog snapshot")
	}
	data, err := os.ReadFile(opts.Base)
	if err != nil {
		return Result{}, errors.New("read base config failed")
	}
	var base map[string]any
	if json.Unmarshal(data, &base) != nil {
		return Result{}, errors.New("decode base config failed")
	}
	values, ok := base["outbounds"].([]any)
	if !ok {
		return Result{}, errors.New("base outbounds must be an array")
	}
	tags := map[string]bool{}
	for _, value := range values {
		item, ok := value.(map[string]any)
		if !ok {
			return Result{}, errors.New("invalid base outbound")
		}
		tag, _ := item["tag"].(string)
		if tag != "" && tags[tag] {
			return Result{}, errors.New("base outbound tag collision")
		}
		tags[tag] = true
	}
	generated := []any{}
	seen := map[string]bool{}
	for i, node := range snapshot.Nodes {
		tag := fmt.Sprintf("catalog-shadow-%d", i+1)
		item, err := nodeuri.ParseURI(node.Uri, tag)
		if err != nil {
			return Result{}, errors.New("catalog node parse failed")
		}
		if tags[tag] {
			return Result{}, errors.New("outbound tag collision")
		}
		tags[tag] = true
		delete(item, "tag")
		fingerprint, _ := json.Marshal(item)
		item["tag"] = tag
		if seen[string(fingerprint)] {
			continue
		}
		seen[string(fingerprint)] = true
		generated = append(generated, tag)
		values = append(values, item)
	}
	for _, value := range values {
		item := value.(map[string]any)
		if item["tag"] == "telegram-auto" || item["tag"] == "default-auto" {
			existing, _ := item["outbounds"].([]any)
			item["outbounds"] = append(append([]any(nil), generated...), existing...)
		}
	}
	base["outbounds"] = values
	data, err = json.MarshalIndent(base, "", "  ")
	if err != nil {
		return Result{}, errors.New("encode candidate failed")
	}
	f, err := os.CreateTemp(filepath.Dir(opts.Output), ".catalog-shadow-*")
	if err != nil {
		return Result{}, errors.New("create candidate failed")
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		return Result{}, errors.New("write candidate failed")
	}
	if f.Sync() != nil || f.Close() != nil {
		return Result{}, errors.New("flush candidate failed")
	}
	validationCtx, validationCancel := context.WithTimeout(ctx, 15*time.Second)
	defer validationCancel()
	if validate(validationCtx, f.Name()) != nil || validationCtx.Err() != nil {
		return Result{}, errors.New("sing-box validation failed")
	}
	if ctx.Err() != nil || !validSnapshot(snapshot, opts.MaxAge, time.Now()) {
		return Result{}, errors.New("snapshot expired before publication")
	}
	if err := CheckOutput(opts.Base, opts.Output); err != nil {
		return Result{}, err
	}
	if os.Chmod(f.Name(), 0o640) != nil {
		return Result{}, errors.New("set candidate permissions failed")
	}
	if os.Rename(f.Name(), opts.Output) != nil {
		return Result{}, errors.New("publish candidate failed")
	}
	// Snapshot IDs are opaque and untrusted. A digest is the only reported version.
	version := sha256.Sum256([]byte(snapshot.Id))
	return Result{Version: hex.EncodeToString(version[:8]), Count: len(generated)}, nil
}
