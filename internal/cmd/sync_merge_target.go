package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	gitpkg "github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/style"
)

var (
	syncMTDryRun   bool
	syncMTRemote   string
	syncMTSource   string
	syncMTTarget   string
	syncMTEscalate bool
	syncMTSeverity string
)

// syncCmd is the parent for branch/data synchronization tooling.
var syncCmd = &cobra.Command{
	Use:     "sync",
	GroupID: GroupWork,
	Short:   "Synchronization tooling for branches and refs",
	Long: `Synchronization helpers that keep related git refs consistent.

Subcommands:
  gt sync merge-target    Fast-forward the refinery merge-target onto main`,
}

var syncMergeTargetCmd = &cobra.Command{
	Use:   "merge-target",
	Short: "Fast-forward the refinery merge-target branch onto main",
	Args:  cobra.NoArgs,
	RunE:  runSyncMergeTarget,
	Long: `Keep the refinery's merge-target branch in sync with main.

The refinery rebases polecat MRs onto merge-target before merging. Polecats
branch off main. If main and merge-target diverge — which happens whenever a
commit lands on main without propagating to merge-target (e.g. a hotfix
cherry-pick) — every MR rebase conflicts on the un-propagated content, and the
queue silently stops draining.

This command resyncs merge-target to main when it is safe to do so:

  - If merge-target is missing, there is nothing to sync (integration-branch
    refinery is not in use) — succeeds silently.
  - If merge-target already equals main, it is in sync — succeeds silently.
  - If merge-target is strictly behind main (every merge-target commit is also
    on main), it is fast-forwarded to main via a refspec push. The working tree
    is never touched.
  - If merge-target has commits that are NOT on main, that is real divergence
    requiring human attention. The command prints a diagnostic and exits
    non-zero (and, with --escalate, files a HIGH escalation).

Safe to run after any push to main, on a schedule, or as a manual operator tool.

EXAMPLES:
  gt sync merge-target                 # Fast-forward merge-target onto main
  gt sync merge-target --dry-run       # Show what would change
  gt sync merge-target --escalate      # File an escalation on real divergence
  gt sync merge-target --source master # Use a non-default source branch`,
}

func init() {
	syncMergeTargetCmd.Flags().BoolVarP(&syncMTDryRun, "dry-run", "n", false, "Show what would change without pushing")
	syncMergeTargetCmd.Flags().StringVar(&syncMTRemote, "remote", "origin", "Remote to read from and push to")
	syncMergeTargetCmd.Flags().StringVar(&syncMTSource, "source", "", "Source branch (default: remote's default branch, e.g. main)")
	syncMergeTargetCmd.Flags().StringVar(&syncMTTarget, "target", "merge-target", "Merge-target branch to fast-forward")
	syncMergeTargetCmd.Flags().BoolVar(&syncMTEscalate, "escalate", false, "File a HIGH escalation if real divergence is detected")
	syncMergeTargetCmd.Flags().StringVar(&syncMTSeverity, "severity", "high", "Escalation severity when --escalate is set (critical, high, medium, low)")

	syncCmd.AddCommand(syncMergeTargetCmd)
	rootCmd.AddCommand(syncCmd)
}

func runSyncMergeTarget(cmd *cobra.Command, args []string) error {
	g := gitpkg.NewGit(".")
	if !g.IsRepo() {
		return fmt.Errorf("not a git repository")
	}

	remote := syncMTRemote
	target := syncMTTarget
	source := syncMTSource
	if source == "" {
		source = g.RemoteDefaultBranch()
	}

	// Refresh remote state so ancestry checks reflect what's actually pushed.
	fmt.Printf("Fetching from %s...\n", style.Dim.Render(remote))
	if err := g.Fetch(remote); err != nil {
		return fmt.Errorf("fetching %s: %w", remote, err)
	}

	sourceRef := remote + "/" + source
	targetRef := remote + "/" + target

	sourceSHA, err := g.Rev(sourceRef)
	if err != nil {
		return fmt.Errorf("source branch %s not found (is --source correct?): %w", sourceRef, err)
	}

	// merge-target may legitimately not exist (integration-branch refinery off).
	targetExists, err := g.RemoteBranchExists(remote, target)
	if err != nil {
		return fmt.Errorf("checking %s: %w", targetRef, err)
	}
	if !targetExists {
		fmt.Printf("%s %s has no %q branch — nothing to sync.\n",
			style.Bold.Render("✓"), remote, target)
		return nil
	}

	targetSHA, err := g.Rev(targetRef)
	if err != nil {
		return fmt.Errorf("resolving %s: %w", targetRef, err)
	}

	if sourceSHA == targetSHA {
		fmt.Printf("%s %s is in sync with %s.\n",
			style.Bold.Render("✓"), target, source)
		return nil
	}

	// Real divergence: any commit on merge-target that is not on main means a
	// fast-forward would discard work. That needs a human, not an auto-resync.
	targetIsAncestor, err := g.IsAncestor(targetRef, sourceRef)
	if err != nil {
		return fmt.Errorf("checking ancestry of %s: %w", targetRef, err)
	}
	if !targetIsAncestor {
		ahead, _ := g.CommitsAhead(sourceRef, targetRef) // commits on target not on source
		return reportMergeTargetDivergence(cmd, remote, source, target, ahead)
	}

	// Safe fast-forward: every merge-target commit is already on main.
	behind, err := g.CommitsAhead(targetRef, sourceRef) // commits on source not on target
	if err != nil {
		return fmt.Errorf("counting commits: %w", err)
	}

	if syncMTDryRun {
		fmt.Printf("%s Would fast-forward %s to %s (%d commit(s) behind).\n",
			style.Warning.Render("~"), target, source, behind)
		return nil
	}

	// Push main's tip onto merge-target. Refspec push updates the remote branch
	// without checking it out, so the working tree is never mutated (and the
	// town-root mutation guard is not triggered). Non-force: git rejects this if
	// it is not a fast-forward, which is a final safety net behind the ancestry
	// check above.
	refspec := fmt.Sprintf("%s:refs/heads/%s", sourceSHA, target)
	if err := g.PushRefspec(remote, refspec, false); err != nil {
		return fmt.Errorf("fast-forwarding %s to %s: %w", target, source, err)
	}

	fmt.Printf("%s Fast-forwarded %s to %s (%d commit(s)).\n",
		style.Bold.Render("✓"), target, source, behind)
	return nil
}

// reportMergeTargetDivergence prints an operator diagnostic for real divergence
// and, when --escalate is set, files an escalation. It always returns a non-zero
// error so callers (operators, schedulers) can detect the unhandled condition.
func reportMergeTargetDivergence(cmd *cobra.Command, remote, source, target string, ahead int) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s has diverged from %s\n\n",
		style.Warning.Render("⚠"), target, source)
	fmt.Fprintf(&b, "  %s has %d commit(s) that are NOT on %s.\n", target, ahead, source)
	fmt.Fprintf(&b, "  A fast-forward would discard them, so this needs a human.\n\n")
	fmt.Fprintf(&b, "  Inspect the divergent commits:\n")
	fmt.Fprintf(&b, "    git log %s/%s ^%s/%s --oneline\n\n", remote, target, remote, source)
	fmt.Fprintf(&b, "  If those commits are genuinely unwanted (e.g. salvage that already\n")
	fmt.Fprintf(&b, "  landed on %s another way), resync after tagging a backup:\n", source)
	fmt.Fprintf(&b, "    git tag %s-pre-resync %s/%s\n", target, remote, target)
	fmt.Fprintf(&b, "    git push %s %s/%s:refs/heads/%s --force-with-lease\n", remote, remote, source, target)
	fmt.Print(b.String())

	if syncMTEscalate {
		description := fmt.Sprintf("merge-target sync: %s diverged from %s (%d unmerged commit(s))", target, source, ahead)
		if err := escalateMergeTargetDivergence(cmd, description); err != nil {
			style.PrintWarning("failed to file escalation: %v", err)
		}
	}

	return fmt.Errorf("%s has diverged from %s — manual intervention required", target, source)
}

// escalateMergeTargetDivergence reuses the standard escalation path (bead +
// routed mail) by configuring the shared escalate flags and invoking runEscalate.
// A stable fingerprint suppresses duplicate escalations across repeated runs.
func escalateMergeTargetDivergence(cmd *cobra.Command, description string) error {
	severity := strings.ToLower(syncMTSeverity)
	if severity == "" {
		severity = "high"
	}

	// Configure the shared escalate command state for a single create.
	escalateSeverity = severity
	escalateReason = "Refinery rebases MRs onto merge-target; while it has diverged from the source branch, every MR rebase conflicts and the queue stops draining. Resync merge-target (see `gt sync merge-target`)."
	escalateSource = "gt sync merge-target"
	escalateFingerprint = "sync-merge-target-divergence"
	escalateRelatedBead = ""
	escalateDryRun = false
	escalateStdin = false
	escalateJSON = false

	return runEscalate(cmd, []string{description})
}
