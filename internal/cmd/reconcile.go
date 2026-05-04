package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/reconcile"
)

// reconcile command flags
var (
	reconcileRig    string
	reconcileSince  string
	reconcileRepo   string
	reconcileBranch string
	reconcileQuiet  bool
)

var reconcileCmd = &cobra.Command{
	Use:   "reconcile",
	Short: "Detect divergences across GT subsystem views (hq-136j)",
	Long: `Detect divergences across the GT data plane's overlapping views of state.

GT maintains 8+ overlapping views (bd state, polecat agent_state, hooked-status,
tmux session, worktree, gt-local refs, refinery queue, origin/main). Each
subsystem trusts its peers — when any view fails, neighboring views report
success based on stale assumptions. The reconciler detects these silently
diverged states and reports them.

Currently implements two of seven planned detection rules (children of hq-136j):

  N1: bd-CLOSED → origin/main commit verification.
      Catches the false-CLOSED-zero-deliverable bug (a polecat closed a bead
      with someone else's commit_sha — most commonly a zombie polecat with
      no worktree closing using HEAD of the rig's main branch).

  N4: HOOKED-bead orphan check.
      Catches HOOKED beads whose assignee polecat is dead, missing, or
      double-bonded (assigned to >1 HOOKED bead simultaneously).

All checks are detect-only and fail-open: an ambiguous signal reports OK
rather than divergence. Output is human-readable to stdout.

Exit codes:
  0  no divergences detected
  1  divergences detected
  2  configuration / runtime error

Examples:
  gt reconcile --rig CIPcodes --since 24h
  gt reconcile --rig CIPcodes --since 1h --branch main
  gt reconcile --quiet                   # suppress per-bead OK lines`,
	RunE: runReconcile,
}

func init() {
	reconcileCmd.Flags().StringVar(&reconcileRig, "rig", "", "Rig name (auto-detected from cwd if omitted)")
	reconcileCmd.Flags().StringVar(&reconcileSince, "since", "24h", "How far back to scan CLOSED beads (e.g. 24h, 1h, 30m)")
	reconcileCmd.Flags().StringVar(&reconcileRepo, "repo", "", "Repo path for git lookups (auto-detected from rig)")
	reconcileCmd.Flags().StringVar(&reconcileBranch, "branch", "origin/main", "Branch to verify CLOSED-bead commits against")
	reconcileCmd.Flags().BoolVar(&reconcileQuiet, "quiet", false, "Suppress per-bead OK lines (only show divergences)")

	rootCmd.AddCommand(reconcileCmd)
}

func runReconcile(cmd *cobra.Command, args []string) error {
	rig := reconcileRig
	if rig == "" {
		rig = autoDetectRig()
	}
	if rig == "" {
		return fmt.Errorf("could not auto-detect rig; pass --rig")
	}

	repo := reconcileRepo
	if repo == "" {
		repo = autoDetectRepoDir(rig)
	}
	if repo == "" {
		return fmt.Errorf("could not locate repo for rig %s; pass --repo", rig)
	}
	if _, err := os.Stat(filepath.Join(repo, ".git")); err != nil {
		return fmt.Errorf("repo not a git directory: %s", repo)
	}

	since, err := parseSinceDuration(reconcileSince)
	if err != nil {
		return fmt.Errorf("--since: %w", err)
	}

	beadsDir := ""
	if d := filepath.Join(repo, ".beads"); dirExists(d) {
		beadsDir = d
	}

	if !reconcileQuiet {
		fmt.Printf("=== gt-reconcile rig=%s repo=%s since=%s branch=%s ===\n",
			rig, repo, reconcileSince, reconcileBranch)
	}

	divergences := 0

	// --- N1: bd-CLOSED → origin/main commit verification ---
	if !reconcileQuiet {
		fmt.Println("\n--- N1: bd-CLOSED → origin/main commit verification ---")
	}
	closed, err := reconcile.ListClosedBeads(repo, since)
	if err != nil {
		return fmt.Errorf("list closed beads: %w", err)
	}
	if len(closed) == 0 && !reconcileQuiet {
		fmt.Printf("  no CLOSED beads since %s\n", since)
	}
	finder := reconcile.GitCmdFinder{RepoDir: repo}
	for _, beadID := range closed {
		r := reconcile.VerifyClosedBead(finder, beadsDir, reconcileBranch, beadID, since)
		if r.Divergent() {
			fmt.Println(r.String())
			divergences++
		} else if !reconcileQuiet {
			fmt.Println(r.String())
		}
	}

	// --- N4: HOOKED-bead orphan check ---
	if !reconcileQuiet {
		fmt.Println("\n--- N4: HOOKED-bead orphan check ---")
	}
	hookedIDs, hookedAssignees, err := reconcile.HookedAssigneesAndIDs(beadsDir, repo)
	if err != nil {
		return fmt.Errorf("list hooked beads: %w", err)
	}
	if len(hookedIDs) == 0 && !reconcileQuiet {
		fmt.Println("  no HOOKED beads")
	}
	registry, err := reconcile.PolecatRegistry(rig)
	if err != nil {
		// Soft-fail: empty registry. Reports all assignees as registry-missing.
		registry = map[string]bool{}
		fmt.Fprintf(os.Stderr, "warning: could not load polecat registry: %v\n", err)
	}
	for _, beadID := range hookedIDs {
		r := reconcile.CheckHookedBead(beadsDir, rig, beadID, hookedAssignees, registry)
		if r.Divergent() {
			fmt.Println(r.String())
			divergences++
		} else if !reconcileQuiet {
			fmt.Println(r.String())
		}
	}

	if !reconcileQuiet {
		fmt.Println()
	}
	if divergences == 0 {
		if !reconcileQuiet {
			fmt.Println("=== reconcile clean: 0 divergences ===")
		}
		return nil
	}
	fmt.Printf("=== reconcile detected %d divergence(s) ===\n", divergences)
	return cobraSilentExit(1)
}

// cobraSilentExit returns an error that produces a non-zero exit code without
// triggering cobra's "Error:" stderr prefix.
func cobraSilentExit(code int) error {
	return &silentExit{code: code}
}

type silentExit struct{ code int }

func (e *silentExit) Error() string { return "" }
func (e *silentExit) ExitCode() int { return e.code }

// autoDetectRig walks up from cwd looking for a .beads dir at a rig root
// (i.e., parent has crew/ sibling). Falls back to env var GT_RIG.
func autoDetectRig() string {
	if r := os.Getenv("GT_RIG"); r != "" {
		return r
	}
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	home, _ := os.UserHomeDir()
	gtRoot := filepath.Join(home, "gt")
	for d := cwd; d != "/" && d != home; d = filepath.Dir(d) {
		// Is this a rig root? It must be a direct child of ~/gt with a .beads dir.
		if filepath.Dir(d) == gtRoot && dirExists(filepath.Join(d, ".beads")) {
			return filepath.Base(d)
		}
	}
	return ""
}

// autoDetectRepoDir picks the crew worktree for the current $USER as the
// authoritative origin/main view, falling back to the rig root.
func autoDetectRepoDir(rig string) string {
	home, _ := os.UserHomeDir()
	user := os.Getenv("USER")
	if user != "" {
		crewRepo := filepath.Join(home, "gt", rig, "crew", user)
		if dirExists(filepath.Join(crewRepo, ".git")) {
			return crewRepo
		}
	}
	rigRoot := filepath.Join(home, "gt", rig)
	if dirExists(filepath.Join(rigRoot, ".git")) {
		return rigRoot
	}
	return ""
}

// parseSinceDuration converts a duration spec (24h, 1h, 30m) to an RFC3339
// timestamp suitable for `bd list --closed-after` and `git log --since`.
func parseSinceDuration(spec string) (string, error) {
	d, err := time.ParseDuration(spec)
	if err != nil {
		// time.ParseDuration doesn't accept "1d"; handle days specially.
		if len(spec) > 1 && spec[len(spec)-1] == 'd' {
			n, perr := time.ParseDuration(spec[:len(spec)-1] + "h")
			if perr != nil {
				return "", err
			}
			// 1d == 24h
			factor := 24
			d = n * time.Duration(factor)
		} else {
			return "", err
		}
	}
	return time.Now().UTC().Add(-d).Format(time.RFC3339), nil
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}
