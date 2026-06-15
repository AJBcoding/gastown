package cmd

import (
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/deps"
)

func resetBeadsVersionCheckForTest(t *testing.T, fn func() (deps.BeadsStatus, string)) {
	t.Helper()

	origCheckBeads := checkBeads

	checkBeads = fn
	cachedVersionCheckResult = nil
	versionCheckOnce = sync.Once{}

	t.Cleanup(func() {
		checkBeads = origCheckBeads
		cachedVersionCheckResult = nil
		versionCheckOnce = sync.Once{}
	})
}

func TestCheckBeadsVersionRejectsVersionsAboveSupportedMaximum(t *testing.T) {
	resetBeadsVersionCheckForTest(t, func() (deps.BeadsStatus, string) {
		return deps.BeadsTooNew, "1.0.5"
	})

	err := CheckBeadsVersion()
	if err == nil {
		t.Fatal("CheckBeadsVersion returned nil for a too-new bd version")
	}
	if !isUnsupportedNewBeadsVersion(err) {
		t.Fatalf("expected unsupported-new marker for error: %v", err)
	}
	if !strings.Contains(err.Error(), "supports at most 1.0.4") {
		t.Fatalf("error %q does not mention supported maximum", err)
	}
	if !strings.Contains(err.Error(), deps.BeadsInstallPath) {
		t.Fatalf("error %q does not include pinned reinstall command", err)
	}
}

func TestCheckBeadsVersionDoesNotHardFailOlderVersionErrors(t *testing.T) {
	resetBeadsVersionCheckForTest(t, func() (deps.BeadsStatus, string) {
		return deps.BeadsTooOld, "1.0.3"
	})

	err := CheckBeadsVersion()
	if err == nil {
		t.Fatal("CheckBeadsVersion returned nil for a too-old bd version")
	}
	if isUnsupportedNewBeadsVersion(err) {
		t.Fatalf("too-old error should remain warning-only, got unsupported-new marker: %v", err)
	}
}

func TestPersistentPreRunFailsForBdAboveSupportedMaximum(t *testing.T) {
	resetBeadsVersionCheckForTest(t, func() (deps.BeadsStatus, string) {
		return deps.BeadsTooNew, "1.0.5"
	})

	for _, use := range []string{"ready"} {
		t.Run(use, func(t *testing.T) {
			err := persistentPreRun(&cobra.Command{Use: use}, nil)
			if err == nil {
				t.Fatal("persistentPreRun returned nil for a too-new bd version")
			}
			if !isUnsupportedNewBeadsVersion(err) {
				t.Fatalf("persistentPreRun returned non-fatal beads error marker: %v", err)
			}
		})
	}
}

func TestPersistentPreRunAllowsBeadsExemptCommandsWithBdAboveSupportedMaximum(t *testing.T) {
	for _, use := range []string{"version", "prime", "install", "doctor", "health", "config"} {
		t.Run(use, func(t *testing.T) {
			called := false
			resetBeadsVersionCheckForTest(t, func() (deps.BeadsStatus, string) {
				called = true
				return deps.BeadsTooNew, "1.0.5"
			})

			if err := persistentPreRun(&cobra.Command{Use: use}, nil); err != nil {
				t.Fatalf("persistentPreRun(%s): %v", use, err)
			}
			if called {
				t.Fatalf("%s should not run bd version checks", use)
			}
		})
	}
}

func TestPersistentPreRunAllowsRoleCommandsWithBdAboveSupportedMaximum(t *testing.T) {
	called := false
	resetBeadsVersionCheckForTest(t, func() (deps.BeadsStatus, string) {
		called = true
		return deps.BeadsTooNew, "1.0.5"
	})

	roleCmd := &cobra.Command{Use: "role"}
	childCmd := &cobra.Command{Use: "inspect"}
	roleCmd.AddCommand(childCmd)

	if err := persistentPreRun(childCmd, nil); err != nil {
		t.Fatalf("persistentPreRun(role inspect): %v", err)
	}
	if called {
		t.Fatal("role commands should not run bd version checks")
	}
}

func TestPersistentPreRunSkipsBdVersionCheckForHotPathCommands(t *testing.T) {
	called := false
	resetBeadsVersionCheckForTest(t, func() (deps.BeadsStatus, string) {
		called = true
		return deps.BeadsTooNew, "1.0.5"
	})

	if err := persistentPreRun(&cobra.Command{Use: "status-line"}, nil); err != nil {
		t.Fatalf("persistentPreRun(status-line): %v", err)
	}
	if called {
		t.Fatal("status-line should not run bd version checks")
	}
}
