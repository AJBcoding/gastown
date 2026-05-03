package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSlingPreflight_NoSentinel verifies absence of the sentinel is a no-op.
func TestSlingPreflight_NoSentinel(t *testing.T) {
	townRoot := t.TempDir()
	if err := slingPreflight(townRoot, false); err != nil {
		t.Errorf("slingPreflight with no sentinel: unexpected error: %v", err)
	}
	if err := slingPreflight(townRoot, true); err != nil {
		t.Errorf("slingPreflight with no sentinel + force: unexpected error: %v", err)
	}
}

// TestSlingPreflight_SentinelBlocks verifies the sentinel refuses dispatch
// and the error message includes the recorded reason.
func TestSlingPreflight_SentinelBlocks(t *testing.T) {
	townRoot := t.TempDir()
	reason := "weekly quota at 92% for account ghosttrack"
	if err := os.WriteFile(filepath.Join(townRoot, noSlingFileName), []byte(reason+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile sentinel: %v", err)
	}

	err := slingPreflight(townRoot, false)
	if err == nil {
		t.Fatal("expected error when sentinel present, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, noSlingFileName) {
		t.Errorf("error should mention sentinel name %q; got: %s", noSlingFileName, msg)
	}
	if !strings.Contains(msg, reason) {
		t.Errorf("error should include reason text %q; got: %s", reason, msg)
	}
	if !strings.Contains(msg, "--force") {
		t.Errorf("error should mention --force override; got: %s", msg)
	}
}

// TestSlingPreflight_ForceBypasses verifies --force returns nil even with
// the sentinel present (with a warning printed to stderr).
func TestSlingPreflight_ForceBypasses(t *testing.T) {
	townRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(townRoot, noSlingFileName), []byte("test reason"), 0o644); err != nil {
		t.Fatalf("WriteFile sentinel: %v", err)
	}

	if err := slingPreflight(townRoot, true); err != nil {
		t.Errorf("slingPreflight with --force should bypass sentinel; got error: %v", err)
	}
}

// TestSlingPreflight_EmptySentinelBlocks verifies an empty sentinel file
// still blocks (operator wrote the file but recorded no reason).
func TestSlingPreflight_EmptySentinelBlocks(t *testing.T) {
	townRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(townRoot, noSlingFileName), nil, 0o644); err != nil {
		t.Fatalf("WriteFile empty sentinel: %v", err)
	}
	err := slingPreflight(townRoot, false)
	if err == nil {
		t.Fatal("expected error with empty sentinel, got nil")
	}
	if !strings.Contains(err.Error(), "no reason recorded") {
		t.Errorf("empty sentinel should report 'no reason recorded'; got: %s", err.Error())
	}
}
