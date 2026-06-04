package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// tempTownRoot returns a temp directory with symlinks resolved, suitable for
// use as a fake town root. On macOS t.TempDir() lives under /var/folders, which
// is a symlink to /private/var/folders. Code under test derives the town root
// from os.Getwd() (which resolves symlinks after os.Chdir, since Chdir does not
// update $PWD), so fixtures that set BEADS_DIR / routing env vars from the raw
// temp path would mismatch the resolved path bd subprocesses actually see —
// causing "wrong database" / "bead not found" failures only on macOS. Resolving
// the temp dir up front keeps both sides in the same (/private) form.
func tempTownRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(t.TempDir()): %v", err)
	}
	return dir
}

// buildGT builds the gt binary and returns its path.
// It caches the build across tests in the same run.
var cachedGTBinary string

func buildGT(t *testing.T) string {
	t.Helper()

	if cachedGTBinary != "" {
		// Verify cached binary still exists
		if _, err := os.Stat(cachedGTBinary); err == nil {
			return cachedGTBinary
		}
		// Binary was cleaned up, rebuild
		cachedGTBinary = ""
	}

	// Find project root (where go.mod is)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get working directory: %v", err)
	}

	// Walk up to find go.mod
	projectRoot := wd
	for {
		if _, err := os.Stat(filepath.Join(projectRoot, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(projectRoot)
		if parent == projectRoot {
			t.Fatal("could not find project root (go.mod)")
		}
		projectRoot = parent
	}

	// Build gt binary to a persistent temp location (not per-test)
	tmpDir := os.TempDir()
	binaryName := "gt-integration-test"
	if runtime.GOOS == "windows" {
		binaryName += ".exe"
	}
	tmpBinary := filepath.Join(tmpDir, binaryName)
	// Must set BuiltProperly=1 via ldflags, otherwise binary refuses to run
	ldflags := "-X github.com/steveyegge/gastown/internal/cmd.BuiltProperly=1"
	cmd := exec.Command("go", "build", "-ldflags", ldflags, "-o", tmpBinary, "./cmd/gt")
	cmd.Dir = projectRoot
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to build gt: %v\nOutput: %s", err, output)
	}

	cachedGTBinary = tmpBinary
	return tmpBinary
}
