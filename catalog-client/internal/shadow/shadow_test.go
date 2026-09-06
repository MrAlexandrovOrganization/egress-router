package shadow

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	catalogv1 "github.com/MrAlexandrovOrganization/egress-router/catalog-client/internal/gen/catalogv1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

const fixtureBase = `{"outbounds":[{"type":"direct","tag":"direct"},{"type":"urltest","tag":"telegram-auto","outbounds":["direct"]},{"type":"urltest","tag":"default-auto","outbounds":["direct"]}],"route":{"final":"default-auto"}}`
const fixtureURI = "trojan://synthetic-secret@proxy.example:443?sni=tls.example"

type server struct {
	catalogv1.UnimplementedCatalogServiceServer
	get func(context.Context, *catalogv1.GetSnapshotRequest) (*catalogv1.Snapshot, error)
}

func (s *server) GetSnapshot(ctx context.Context, req *catalogv1.GetSnapshotRequest) (*catalogv1.Snapshot, error) {
	return s.get(ctx, req)
}

func client(t *testing.T, get func(context.Context, *catalogv1.GetSnapshotRequest) (*catalogv1.Snapshot, error)) catalogv1.CatalogServiceClient {
	t.Helper()
	lis := bufconn.Listen(1024 * 1024)
	s := grpc.NewServer()
	catalogv1.RegisterCatalogServiceServer(s, &server{get: get})
	go func() { _ = s.Serve(lis) }()
	t.Cleanup(func() { s.Stop(); _ = lis.Close() })
	conn, err := grpc.NewClient("passthrough:///bufnet", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return catalogv1.NewCatalogServiceClient(conn)
}

func snapshot() *catalogv1.Snapshot {
	now := time.Now().Unix()
	return &catalogv1.Snapshot{Id: "synthetic-secret", Profile: "server", CreatedAtUnix: now, ValidUntilUnix: now + 300, Nodes: []*catalogv1.Node{{Uri: fixtureURI, SchemaVersion: 1, LastSuccessUnix: now}}}
}

func options(t *testing.T) Options {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{Base: filepath.Join(dir, "base.json"), Output: filepath.Join(dir, "catalog-shadow.json"), Token: "synthetic-token", MaxAge: time.Hour}
	if err := os.WriteFile(opts.Base, []byte(fixtureBase), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(opts.Output, []byte("previous"), 0o640); err != nil {
		t.Fatal(err)
	}
	return opts
}

func TestRunBufconn(t *testing.T) {
	opts := options(t)
	s := snapshot()
	s.Nodes = append(s.Nodes, s.Nodes[0])
	c := client(t, func(ctx context.Context, req *catalogv1.GetSnapshotRequest) (*catalogv1.Snapshot, error) {
		md, _ := metadata.FromIncomingContext(ctx)
		if got := md.Get("authorization"); len(got) != 1 || got[0] != "Bearer synthetic-token" {
			t.Error("missing auth")
		}
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 15*time.Second {
			t.Error("missing deadline")
		}
		if req.Profile != "server" || req.Limit != 50 || req.MaxAgeSeconds != 3600 {
			t.Error("incorrect request")
		}
		return s, nil
	})
	validated := false
	result, err := Run(context.Background(), c, opts, func(ctx context.Context, path string) error {
		validated = true
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 15*time.Second {
			t.Error("missing validation deadline")
		}
		if path == opts.Output {
			t.Error("validation must precede publication")
		}
		old, _ := os.ReadFile(opts.Output)
		if string(old) != "previous" {
			t.Error("published before validation")
		}
		return nil
	})
	if err != nil || !validated || result.Count != 1 || len(result.Version) != 16 || strings.Contains(result.Version, "secret") {
		t.Fatalf("bad result: %+v %v", result, err)
	}
	data, err := os.ReadFile(opts.Output)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if json.Unmarshal(data, &cfg) != nil {
		t.Fatal("invalid JSON")
	}
	out := cfg["outbounds"].([]any)
	if len(out) != 4 {
		t.Fatal("deduplication failed")
	}
	for _, i := range []int{1, 2} {
		refs := out[i].(map[string]any)["outbounds"].([]any)
		if len(refs) != 2 || refs[0] != "catalog-shadow-1" || refs[1] != "direct" {
			t.Fatal("selector merge failed")
		}
	}
	info, _ := os.Stat(opts.Output)
	if info.Mode().Perm() != 0o640 {
		t.Fatal("wrong permissions")
	}
	base, _ := os.ReadFile(opts.Base)
	if string(base) != fixtureBase {
		t.Fatal("base changed")
	}
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(opts.Output), ".catalog-shadow-*"))
	if len(matches) != 0 {
		t.Fatal("temporary leaked")
	}
}

func TestFailuresPreserveOutput(t *testing.T) {
	for _, kind := range []string{"rpc", "expired", "old-created", "future-created", "empty", "profile", "too-many", "old-node", "future-node", "unknown-schema", "bad-uri", "bad-base", "collision", "validator", "expires-during-validation", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			opts := options(t)
			s := snapshot()
			switch kind {
			case "expired":
				s.ValidUntilUnix = time.Now().Unix()
			case "old-created":
				s.CreatedAtUnix -= 7200
			case "future-created":
				s.CreatedAtUnix += 60
			case "empty":
				s.Nodes = nil
			case "profile":
				s.Profile = "desktop"
			case "too-many":
				for len(s.Nodes) <= 50 {
					s.Nodes = append(s.Nodes, s.Nodes[0])
				}
			case "old-node":
				s.Nodes[0].LastSuccessUnix -= 7200
			case "future-node":
				s.Nodes[0].LastSuccessUnix += 60
			case "unknown-schema":
				s.Nodes[0].SchemaVersion = 2
			case "bad-uri":
				s.Nodes[0].Uri = "vmess://synthetic-secret"
			case "bad-base":
				if err := os.WriteFile(opts.Base, []byte("synthetic-secret"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "collision":
				if err := os.WriteFile(opts.Base, []byte(`{"outbounds":[{"tag":"catalog-shadow-1"}]}`), 0o600); err != nil {
					t.Fatal(err)
				}
			case "expires-during-validation":
				s.ValidUntilUnix = time.Now().Unix() + 1
			}
			c := client(t, func(context.Context, *catalogv1.GetSnapshotRequest) (*catalogv1.Snapshot, error) {
				if kind == "rpc" {
					return nil, status.Error(codes.Unauthenticated, "synthetic-secret")
				}
				return s, nil
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			_, err := Run(ctx, c, opts, func(context.Context, string) error {
				if kind == "validator" {
					return errors.New("synthetic-secret")
				}
				if kind == "expires-during-validation" {
					time.Sleep(1100 * time.Millisecond)
				}
				if kind == "cancel" {
					cancel()
				}
				return nil
			})
			if err == nil || strings.Contains(err.Error(), "synthetic-secret") {
				t.Fatal("missing or unsafe error")
			}
			data, _ := os.ReadFile(opts.Output)
			if string(data) != "previous" {
				t.Fatal("output changed")
			}
			matches, _ := filepath.Glob(filepath.Join(filepath.Dir(opts.Output), ".catalog-shadow-*"))
			if len(matches) != 0 {
				t.Fatal("temporary leaked")
			}
		})
	}
}

func TestSnapshotValidation(t *testing.T) {
	now := time.Now()
	if validSnapshot(nil, time.Hour, now) {
		t.Fatal("nil accepted")
	}
	s := snapshot()
	s.Nodes = []*catalogv1.Node{nil}
	if validSnapshot(s, time.Hour, now) {
		t.Fatal("nil node accepted")
	}
	s = snapshot()
	s.ValidUntilUnix = now.Unix() + 1
	if validSnapshot(s, time.Hour, now.Add(2*time.Second)) {
		t.Fatal("expired accepted")
	}
}

func TestOutputGuards(t *testing.T) {
	opts := options(t)
	dir := filepath.Dir(opts.Base)
	for _, name := range []string{"base.json", "config.json", "active.json", "current.json", "previous.json", "generated.json"} {
		if CheckOutput(opts.Base, filepath.Join(dir, name)) == nil {
			t.Fatalf("accepted %s", name)
		}
	}
	if CheckOutput(opts.Output, opts.Output) == nil {
		t.Fatal("accepted base overwrite")
	}
	if err := os.Remove(opts.Output); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(opts.Base, opts.Output); err != nil {
		t.Fatal(err)
	}
	if CheckOutput(opts.Base, opts.Output) == nil {
		t.Fatal("accepted symlink")
	}
	if err := os.Remove(opts.Output); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(opts.Base, opts.Output); err != nil {
		t.Fatal(err)
	}
	if CheckOutput(opts.Base, opts.Output) == nil {
		t.Fatal("accepted hardlink to base")
	}
	alias := filepath.Join(dir, "alias")
	if err := os.Symlink(dir, alias); err != nil {
		t.Fatal(err)
	}
	if CheckOutput(opts.Base, filepath.Join(alias, "catalog-shadow.json")) == nil {
		t.Fatal("accepted directory symlink")
	}
}

func TestDialPolicy(t *testing.T) {
	for _, tc := range []struct {
		addr               string
		allow, trusted, ok bool
	}{
		{"catalog.example:443", false, false, true}, {"localhost:8080", true, false, true}, {"127.0.0.1:8080", true, false, true}, {"[::1]:8080", true, false, true}, {"catalog.example:8080", true, false, false}, {"catalog.example:8080", true, true, true}, {"catalog.example:443", false, true, false}, {"https://catalog.example:443", false, false, false}, {"catalog.example:0", false, false, false},
	} {
		c, err := Dial(tc.addr, "synthetic-token", tc.allow, tc.trusted)
		if c != nil {
			_ = c.Close()
		}
		if (err == nil) != tc.ok {
			t.Fatalf("policy mismatch: %+v", tc)
		}
	}
}

func TestToken(t *testing.T) {
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte("synthetic-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if value, err := Token("", file); err != nil || value != "synthetic-token" {
		t.Fatal("file token failed")
	}
	for _, pair := range [][2]string{{"", ""}, {"synthetic-token", file}, {"bad\nsecret", ""}, {"", file + "-missing"}, {strings.Repeat("x", 8193), ""}} {
		if _, err := Token(pair[0], pair[1]); err == nil {
			t.Fatal("invalid token accepted")
		}
	}
}

func TestDialDoesNotDowngradeTLS(t *testing.T) {
	// A plaintext fake must not receive credentials when TLS is the default.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := grpc.NewServer()
	catalogv1.RegisterCatalogServiceServer(s, &server{get: func(ctx context.Context, _ *catalogv1.GetSnapshotRequest) (*catalogv1.Snapshot, error) {
		md, _ := metadata.FromIncomingContext(ctx)
		if len(md.Get("authorization")) != 1 {
			t.Error("missing explicit plaintext credentials")
		}
		return snapshot(), nil
	}})
	go func() { _ = s.Serve(lis) }()
	t.Cleanup(func() { s.Stop(); _ = lis.Close() })
	for _, plaintext := range []bool{false, true} {
		conn, err := Dial(lis.Addr().String(), "synthetic-token", plaintext, false)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer synthetic-token")
		_, err = catalogv1.NewCatalogServiceClient(conn).GetSnapshot(ctx, &catalogv1.GetSnapshotRequest{Profile: "server"})
		cancel()
		_ = conn.Close()
		if (err == nil) != plaintext {
			t.Fatal("TLS default or explicit plaintext policy violated")
		}
	}
}

func TestValidatorFakeExecutable(t *testing.T) {
	// The Go test binary acts as the validator; no application runs on the host.
	t.Setenv("CATALOG_VALIDATOR_HELPER", "1")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(context.Background(), exe, "fixture-only"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if Validate(ctx, exe, "fixture-only") == nil {
		t.Fatal("cancel ignored")
	}
	if Validate(context.Background(), filepath.Join(t.TempDir(), "missing"), "fixture-only") == nil {
		t.Fatal("missing validator accepted")
	}
}

func TestMain(m *testing.M) {
	if os.Getenv("CATALOG_VALIDATOR_HELPER") == "1" {
		if len(os.Args) == 4 && os.Args[1] == "check" && os.Args[2] == "-c" && os.Args[3] == "fixture-only" {
			os.Exit(0)
		}
		os.Exit(2)
	}
	os.Exit(m.Run())
}

func TestIntegrationGeneratedConfig(t *testing.T) {
	image := os.Getenv("SING_BOX_TEST_IMAGE")
	if image == "" {
		t.Skip("set SING_BOX_TEST_IMAGE for fixture-only Docker validation")
	}
	publicKey := base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	for i, uri := range []string{
		"vless://550e8400-e29b-41d4-a716-446655440000@proxy.example:443?security=tls&type=ws&path=%2Ffixture&sni=tls.example",
		"vless://550e8400-e29b-41d4-a716-446655440000@proxy.example:443?security=reality&type=grpc&serviceName=fixture&pbk=" + publicKey + "&sid=0123456789abcdef",
		fixtureURI, "hy2://synthetic-secret@proxy.example:443?sni=tls.example&obfs=salamander&obfs-password=fixture",
		"ss://" + base64.RawURLEncoding.EncodeToString([]byte("chacha20-ietf-poly1305:fixture")) + "@proxy.example:8388",
	} {
		t.Run(string(rune('A'+i)), func(t *testing.T) {
			opts := options(t)
			s := snapshot()
			s.Nodes[0].Uri = uri
			c := client(t, func(context.Context, *catalogv1.GetSnapshotRequest) (*catalogv1.Snapshot, error) { return s, nil })
			_, err := Run(context.Background(), c, opts, func(ctx context.Context, path string) error {
				f, err := os.Open(path)
				if err != nil {
					return err
				}
				defer f.Close()
				cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "--network=none", "-i", image, "check", "-c", "/dev/stdin")
				cmd.Stdin = f
				output, err := cmd.CombinedOutput()
				if err != nil {
					t.Logf("synthetic fixture diagnostics: %s", output)
				}
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
