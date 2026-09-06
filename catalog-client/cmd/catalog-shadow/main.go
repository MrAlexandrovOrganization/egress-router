package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	catalogv1 "github.com/MrAlexandrovOrganization/egress-router/catalog-client/internal/gen/catalogv1"
	"github.com/MrAlexandrovOrganization/egress-router/catalog-client/internal/shadow"
)

func run(ctx context.Context, args []string, getenv func(string) string) (shadow.Result, error) {
	flags := flag.NewFlagSet("catalog-shadow", flag.ContinueOnError)
	flags.SetOutput(io.Discard) // flag errors can echo secret-bearing arguments.
	base := flags.String("base", "", "required local base config (read-only)")
	output := flags.String("output", "runtime/catalog-shadow.json", "dedicated shadow filename only")
	maxAge := flags.Duration("max-age", time.Hour, "maximum snapshot/node age")
	singBox := flags.String("sing-box", "sing-box", "validator executable")
	allowInsecure := flags.Bool("allow-insecure", false, "explicit plaintext opt-in")
	trusted := flags.Bool("trusted-network", false, "also allow plaintext on an operator-trusted non-loopback network")
	if flags.Parse(args) != nil || flags.NArg() != 0 {
		return shadow.Result{}, errors.New("invalid command arguments")
	}
	if err := shadow.CheckOutput(*base, *output); err != nil {
		return shadow.Result{}, err
	}
	if *maxAge < time.Second || *singBox == "" {
		return shadow.Result{}, errors.New("invalid shadow options")
	}
	token, err := shadow.Token(getenv("CATALOG_TOKEN"), getenv("CATALOG_TOKEN_FILE"))
	if err != nil {
		return shadow.Result{}, err
	}
	conn, err := shadow.Dial(getenv("CATALOG_ADDR"), token, *allowInsecure, *trusted)
	if err != nil {
		return shadow.Result{}, err
	}
	defer conn.Close()
	return shadow.Run(ctx, catalogv1.NewCatalogServiceClient(conn), shadow.Options{Base: *base, Output: *output, MaxAge: *maxAge, Token: token}, func(ctx context.Context, path string) error { return shadow.Validate(ctx, *singBox, path) })
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service.name", "catalog-shadow", "mode", "shadow")
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	result, err := run(ctx, os.Args[1:], os.Getenv)
	if err != nil {
		logger.ErrorContext(ctx, "shadow failed", "error", err.Error())
		os.Exit(1)
	}
	logger.InfoContext(ctx, "shadow candidate validated", "version", result.Version, "count", result.Count)
}
