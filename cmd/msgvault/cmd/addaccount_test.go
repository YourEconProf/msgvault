package cmd

import (
	"testing"

	"github.com/wesm/msgvault/internal/store"
)

func TestHasGmailSource(t *testing.T) {
	tmpDir := t.TempDir()
	s, err := store.Open(tmpDir + "/msgvault.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = s.Close() }()
	if err := s.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	const email = "user@company.com"

	// No sources at all — should suggest add-account.
	if hasGmailSource(s, email) {
		t.Error("hasGmailSource should be false with no sources")
	}

	// Non-Gmail source exists — should still suggest add-account.
	if _, err := s.GetOrCreateSource("mbox", email); err != nil {
		t.Fatalf("create mbox source: %v", err)
	}
	if hasGmailSource(s, email) {
		t.Error("hasGmailSource should be false with only mbox source")
	}

	// Gmail source exists — should suppress the hint.
	if _, err := s.GetOrCreateSource("gmail", email); err != nil {
		t.Fatalf("create gmail source: %v", err)
	}
	if !hasGmailSource(s, email) {
		t.Error("hasGmailSource should be true with gmail source")
	}
}
