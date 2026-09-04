package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aicix-labs/factoryd/internal/config"
	"github.com/aicix-labs/factoryd/internal/scm"
)

// The verdict verb reads as the operator principal when one is configured
// -- the proven-distinct driver, never a re-read -- and as the reviewer
// otherwise; never as the producer (#43).
func TestVerdictDriverIsTheOperatorWhenConfiguredElseTheReviewer(t *testing.T) {
	root := t.TempDir()
	write := func(name, tok string) config.CredentialRef {
		p := filepath.Join(root, name)
		if err := os.WriteFile(p, []byte(tok+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return config.CredentialRef{File: p}
	}
	var closes []string
	closed := false
	build := func(cfg *config.Config, token string) (scm.Driver, error) {
		return tokenDriver{token: token, closes: &closes, closed: &closed}, nil
	}
	cfg := &config.Config{Credentials: config.Credentials{Producer: write("p", "p"), Reviewer: write("r", "r")}}
	d, err := verdictDriver(context.Background(), cfg, build)
	if err != nil || d.(tokenDriver).token != "r" {
		t.Fatalf("no operator configured: driver=%v err=%v; want the reviewer's", d, err)
	}
	cfg.Credentials.Operator = write("o", "o")
	d, err = verdictDriver(context.Background(), cfg, build)
	if err != nil || d.(tokenDriver).token != "o" {
		t.Fatalf("operator configured: driver=%v err=%v; want the operator's", d, err)
	}
	cfg.Credentials.Operator = write("o", "r") // one authority behind two paths
	if _, err := verdictDriver(context.Background(), cfg, build); err == nil {
		t.Fatal("an operator that is the reviewer by id was accepted")
	}
}
