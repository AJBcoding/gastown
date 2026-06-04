package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	gitpkg "github.com/steveyegge/gastown/internal/git"
)

// setupSyncRepo creates a bare remote plus a local clone with main pushed, and
// returns the local working directory and the main branch name.
func setupSyncRepo(t *testing.T) (localDir, mainBranch string) {
	t.Helper()

	remoteDir := t.TempDir()
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run(remoteDir, "init", "--bare")

	localDir = t.TempDir()
	if resolved, err := filepath.EvalSymlinks(localDir); err == nil {
		localDir = resolved
	}
	run(localDir, "init")
	run(localDir, "config", "user.email", "test@test.com")
	run(localDir, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(localDir, "README.md"), []byte("# Test\n"), 0644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	run(localDir, "add", ".")
	run(localDir, "commit", "-m", "initial")
	run(localDir, "remote", "add", "origin", remoteDir)

	mainBranch, _ = gitpkg.NewGit(localDir).CurrentBranch()
	run(localDir, "push", "-u", "origin", mainBranch)
	return localDir, mainBranch
}

// resetSyncFlags restores the package-level flags to test defaults.
func resetSyncFlags(t *testing.T, source string) {
	t.Helper()
	syncMTDryRun = false
	syncMTRemote = "origin"
	syncMTSource = source
	syncMTTarget = "merge-target"
	syncMTEscalate = false
	syncMTSeverity = "high"
}

func TestSyncMergeTarget_MissingTarget(t *testing.T) {
	localDir, mainBranch := setupSyncRepo(t)
	t.Chdir(localDir)
	resetSyncFlags(t, mainBranch)

	// merge-target was never created — nothing to sync, no error.
	if err := runSyncMergeTarget(syncMergeTargetCmd, nil); err != nil {
		t.Fatalf("expected nil for missing merge-target, got: %v", err)
	}
}

func TestSyncMergeTarget_UpToDate(t *testing.T) {
	localDir, mainBranch := setupSyncRepo(t)
	g := gitpkg.NewGit(localDir)
	mainSHA, _ := g.Rev("HEAD")
	if err := g.PushRefspec("origin", mainSHA+":refs/heads/merge-target", false); err != nil {
		t.Fatalf("seed merge-target: %v", err)
	}

	t.Chdir(localDir)
	resetSyncFlags(t, mainBranch)

	if err := runSyncMergeTarget(syncMergeTargetCmd, nil); err != nil {
		t.Fatalf("expected nil when in sync, got: %v", err)
	}
}

func TestSyncMergeTarget_FastForward(t *testing.T) {
	localDir, mainBranch := setupSyncRepo(t)
	g := gitpkg.NewGit(localDir)

	// Seed merge-target at the initial commit.
	initialSHA, _ := g.Rev("HEAD")
	if err := g.PushRefspec("origin", initialSHA+":refs/heads/merge-target", false); err != nil {
		t.Fatalf("seed merge-target: %v", err)
	}

	// Advance main (simulates a cherry-pick / hotfix landing on main).
	if err := os.WriteFile(filepath.Join(localDir, "hotfix.txt"), []byte("fix"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = localDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("add", ".")
	run("commit", "-m", "hotfix")
	run("push", "origin", mainBranch)
	newSHA, _ := g.Rev("HEAD")

	t.Chdir(localDir)
	resetSyncFlags(t, mainBranch)

	if err := runSyncMergeTarget(syncMergeTargetCmd, nil); err != nil {
		t.Fatalf("fast-forward sync failed: %v", err)
	}

	tip, err := g.RemoteBranchTip("origin", "merge-target")
	if err != nil {
		t.Fatalf("RemoteBranchTip: %v", err)
	}
	if tip != newSHA {
		t.Errorf("merge-target tip = %s, want %s (main)", tip, newSHA)
	}
}

func TestSyncMergeTarget_DryRunDoesNotPush(t *testing.T) {
	localDir, mainBranch := setupSyncRepo(t)
	g := gitpkg.NewGit(localDir)
	initialSHA, _ := g.Rev("HEAD")
	if err := g.PushRefspec("origin", initialSHA+":refs/heads/merge-target", false); err != nil {
		t.Fatalf("seed merge-target: %v", err)
	}

	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = localDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(localDir, "hotfix.txt"), []byte("fix"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	run("add", ".")
	run("commit", "-m", "hotfix")
	run("push", "origin", mainBranch)

	t.Chdir(localDir)
	resetSyncFlags(t, mainBranch)
	syncMTDryRun = true

	if err := runSyncMergeTarget(syncMergeTargetCmd, nil); err != nil {
		t.Fatalf("dry-run failed: %v", err)
	}

	tip, _ := g.RemoteBranchTip("origin", "merge-target")
	if tip != initialSHA {
		t.Errorf("dry-run mutated merge-target: tip = %s, want %s", tip, initialSHA)
	}
}

func TestSyncMergeTarget_DivergenceReturnsError(t *testing.T) {
	localDir, mainBranch := setupSyncRepo(t)
	g := gitpkg.NewGit(localDir)

	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = localDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	// merge-target gets a commit that never lands on main.
	if err := g.CreateBranch("merge-target"); err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	run("checkout", "merge-target")
	if err := os.WriteFile(filepath.Join(localDir, "only-mt.txt"), []byte("x"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	run("add", ".")
	run("commit", "-m", "divergent")
	run("push", "origin", "merge-target")
	run("checkout", mainBranch)

	// main advances independently.
	if err := os.WriteFile(filepath.Join(localDir, "main.txt"), []byte("y"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	run("add", ".")
	run("commit", "-m", "main work")
	run("push", "origin", mainBranch)

	t.Chdir(localDir)
	resetSyncFlags(t, mainBranch)
	// --escalate stays false so the test does not touch the beads/mail subsystem.

	mtTipBefore, _ := g.RemoteBranchTip("origin", "merge-target")

	err := runSyncMergeTarget(syncMergeTargetCmd, nil)
	if err == nil {
		t.Fatal("expected divergence error, got nil")
	}

	// Divergence must NOT mutate merge-target.
	mtTipAfter, _ := g.RemoteBranchTip("origin", "merge-target")
	if mtTipBefore != mtTipAfter {
		t.Errorf("divergence path mutated merge-target: before %s, after %s", mtTipBefore, mtTipAfter)
	}
}
