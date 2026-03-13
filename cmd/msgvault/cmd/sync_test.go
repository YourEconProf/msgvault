package cmd

import (
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/wesm/msgvault/internal/config"
	"github.com/wesm/msgvault/internal/store"
)

// TestSyncCmd_DuplicateIdentifierRoutesCorrectly verifies that when
// Gmail and IMAP sources share the same identifier, the single-arg
// sync path resolves both and routes each to the correct backend.
//
// Regression test: before the fix, GetSourceByIdentifier returned
// an arbitrary single row, so one source type would be lost.
func TestSyncCmd_DuplicateIdentifierRoutesCorrectly(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/msgvault.db"

	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := s.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	// Create both gmail and imap sources for the same identifier.
	_, err = s.GetOrCreateSource("gmail", "shared@example.com")
	if err != nil {
		t.Fatalf("create gmail source: %v", err)
	}
	_, err = s.GetOrCreateSource("imap", "shared@example.com")
	if err != nil {
		t.Fatalf("create imap source: %v", err)
	}
	_ = s.Close()

	savedCfg := cfg
	savedLogger := logger
	defer func() {
		cfg = savedCfg
		logger = savedLogger
	}()

	cfg = &config.Config{
		HomeDir: tmpDir,
		Data:    config.DataConfig{DataDir: tmpDir},
	}
	logger = slog.New(slog.NewTextHandler(os.Stderr, nil))

	// Create a fresh command with the same RunE to avoid
	// re-parenting the global syncIncrementalCmd.
	testCmd := &cobra.Command{
		Use:  "sync [email]",
		Args: cobra.MaximumNArgs(1),
		RunE: syncIncrementalCmd.RunE,
	}

	root := newTestRootCmd()
	root.AddCommand(testCmd)
	root.SetArgs([]string{"sync", "shared@example.com"})

	err = root.Execute()
	if err == nil {
		t.Fatal("expected error (no credentials/config)")
	}

	errMsg := err.Error()

	// Should NOT hit the legacy Gmail-only fallback, which sets
	// source to nil and produces "no source found".
	if strings.Contains(errMsg, "no source found") {
		t.Error("should not hit legacy Gmail-only fallback path")
	}

	// Both sources should be resolved and attempted, producing
	// 2 failures (IMAP: missing config, Gmail: missing OAuth).
	if !strings.Contains(errMsg, "2 account(s) failed") {
		t.Errorf(
			"expected both sources resolved; got: %s",
			errMsg,
		)
	}
}

// TestSyncCmd_SingleSourceNoAmbiguity verifies that a single
// source for an identifier works without the legacy fallback.
func TestSyncCmd_SingleSourceNoAmbiguity(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/msgvault.db"

	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := s.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	_, err = s.GetOrCreateSource("imap", "solo@example.com")
	if err != nil {
		t.Fatalf("create imap source: %v", err)
	}
	_ = s.Close()

	savedCfg := cfg
	savedLogger := logger
	defer func() {
		cfg = savedCfg
		logger = savedLogger
	}()

	cfg = &config.Config{
		HomeDir: tmpDir,
		Data:    config.DataConfig{DataDir: tmpDir},
	}
	logger = slog.New(slog.NewTextHandler(os.Stderr, nil))

	testCmd := &cobra.Command{
		Use:  "sync [email]",
		Args: cobra.MaximumNArgs(1),
		RunE: syncIncrementalCmd.RunE,
	}

	root := newTestRootCmd()
	root.AddCommand(testCmd)
	root.SetArgs([]string{"sync", "solo@example.com"})

	err = root.Execute()
	if err == nil {
		t.Fatal("expected error (no IMAP config)")
	}

	errMsg := err.Error()

	// Exactly 1 source should fail (IMAP with missing config).
	if !strings.Contains(errMsg, "1 account(s) failed") {
		t.Errorf(
			"expected 1 failed account; got: %s",
			errMsg,
		)
	}

	// Should NOT hit legacy fallback (source exists in DB).
	if strings.Contains(errMsg, "no source found") {
		t.Error("should not hit legacy fallback path")
	}
}
