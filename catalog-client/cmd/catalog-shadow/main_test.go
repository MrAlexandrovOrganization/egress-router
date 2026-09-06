package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestInvalidArgumentsDoNotReadEnvironment(t *testing.T) {
	for _, args := range [][]string{{"--token=synthetic-secret"}, {"--max-age=synthetic-secret"}, {"positional-secret"}, {"--base=base.json", "--output=runtime/active.json"}, {}} {
		_, err := run(context.Background(), args, func(string) string { t.Fatal("environment read before path validation"); return "" })
		if err == nil || strings.Contains(err.Error(), "synthetic-secret") {
			t.Fatal("missing or unsafe error")
		}
	}
}

func TestCLIRequiresToken(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, err = run(context.Background(), []string{"--base=" + filepath.Join(dir, "base.json"), "--output=" + filepath.Join(dir, "catalog-shadow.json")}, func(string) string { return "" })
	if err == nil || err.Error() != "catalog token required" {
		t.Fatalf("unexpected result: %v", err)
	}
}
