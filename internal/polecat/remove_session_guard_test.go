package polecat

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/tmux"
)

// TestRemoveWithOptions_BlocksLiveSession verifies the active-session guard
// (gt-jb6): force-removing a polecat whose tmux session is still healthy must be
// refused with ErrSessionActive, and must happen BEFORE any worktree or agent-bead
// mutation. This is the regression test for the incident where a witness ran
// `gt polecat remove --force` against a live polecat and destroyed its worktree.
func TestRemoveWithOptions_BlocksLiveSession(t *testing.T) {
	requireTmux(t)

	root := t.TempDir()
	r := &rig.Rig{Name: "testrig", Path: root}

	// exists() only checks polecats/<name>/ — create it so the guard is reached.
	const name = "toast"
	polecatDir := filepath.Join(root, "polecats", name)
	if err := os.MkdirAll(polecatDir, 0o755); err != nil {
		t.Fatalf("mkdir polecat dir: %v", err)
	}

	tm := tmux.NewTmux()
	m := NewManager(r, git.NewGit(root), tm)

	// Start a healthy session under the polecat's session name. GT_PROCESS_NAMES=sleep
	// makes the running `sleep` count as the live agent process, so the session is
	// reported SessionHealthy (IsRunning == true).
	sessionName := NewSessionManager(tm, r).SessionName(name)
	if err := tm.NewSessionWithCommandAndEnv(sessionName, "", "sleep 600", map[string]string{
		"GT_PROCESS_NAMES": "sleep",
	}); err != nil {
		t.Fatalf("start live session: %v", err)
	}
	t.Cleanup(func() { _ = tm.KillSessionWithProcesses(sessionName) })

	// Precondition: the session must be reported running, else the test proves nothing.
	if running, _ := NewSessionManager(tm, r).IsRunning(name); !running {
		t.Fatal("precondition failed: session not reported as running")
	}

	// force=true must NOT bypass the live-session guard.
	if err := m.RemoveWithOptions(name, true, false, false); !errors.Is(err, ErrSessionActive) {
		t.Fatalf("RemoveWithOptions(force) = %v, want ErrSessionActive", err)
	}

	// The worktree dir must be untouched — the guard runs before any deletion.
	if _, err := os.Stat(polecatDir); err != nil {
		t.Errorf("polecat dir was removed despite live session: %v", err)
	}
}

// TestRemoveWithOptions_NilTmuxSkipsGuard verifies the active-session guard is
// skipped when the Manager has no tmux handle (e.g. town-wide shutdown listing,
// unit tests). Without a way to verify liveness, the guard must not block — and
// must not panic on the nil tmux. ErrSessionActive must never be returned here.
func TestRemoveWithOptions_NilTmuxSkipsGuard(t *testing.T) {
	root := t.TempDir()
	r := &rig.Rig{Name: "testrig", Path: root}

	const name = "toast"
	if err := os.MkdirAll(filepath.Join(root, "polecats", name), 0o755); err != nil {
		t.Fatalf("mkdir polecat dir: %v", err)
	}

	// nil tmux: guard must be skipped (and must not panic).
	m := NewManager(r, git.NewGit(root), nil)

	// nuclear=true keeps this Dolt-free: it bypasses the uncommitted-work / MR checks
	// and exercises the removal path past the guard. The result may be any non-guard
	// outcome — the only assertion is that the guard did NOT fire.
	if err := m.RemoveWithOptions(name, true, true, false); errors.Is(err, ErrSessionActive) {
		t.Fatalf("RemoveWithOptions(nil tmux) returned ErrSessionActive, want guard skipped: %v", err)
	}
}
