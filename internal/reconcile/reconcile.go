// Package reconcile implements divergence detection across the GT data plane.
//
// GT maintains 8+ overlapping views of system state (bd state, polecat
// agent_state, work-bead hooked-status, polecat tmux session, polecat worktree
// git, gt-local refs, refinery queue, origin/main). Each subsystem trusts its
// peers. When any subsystem fails, neighboring views report success based on
// stale or wrong assumptions, and the optimism propagates outward.
//
// This package implements two of the seven planned detection rules from the
// hq-136j epic:
//
//   - N1 (VerifyClosedBead): bd-CLOSED beads must have a commit on origin/main
//     whose subject references the bead-id. Catches the false-CLOSED-zero-deliverable
//     bug where a polecat closed a bead with someone else's commit_sha.
//
//   - N4 (CheckOrphan): bd-HOOKED beads must have a healthy assignee polecat
//     (registry + worktree + tmux session + no double-bond). Catches the
//     zombie-polecat-still-bonded-to-bead pattern.
//
// All detection is fail-open: when a check is ambiguous (missing data, unparseable
// timestamps, transient errors) the bead is reported OK rather than ORPHAN. False
// negatives are preferred over false positives, since the latter would erode
// operator trust.
package reconcile

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// Status enumerates the verdict for a single bead.
type Status string

const (
	StatusOK          Status = "OK"
	StatusOKVia       Status = "OK*"          // matched via close_reason claim, not subject grep
	StatusWarn        Status = "WARN"         // legitimate no-code close (planning bead)
	StatusMissing     Status = "MISSING"      // closed but no commit references bead
	StatusFalseClosed Status = "FALSE_CLOSED" // close_reason claims sha but sha is unrelated
	StatusOrphan      Status = "ORPHAN"       // hooked bead with dead/missing assignee
)

// Result is the verdict for a single bead.
type Result struct {
	BeadID  string
	Status  Status
	Detail  string // human-readable explanation
	Commit  string // commit-sha if matched
	Subject string // commit subject if matched
}

// Divergent reports whether the result is an actionable divergence (not OK/WARN).
func (r Result) Divergent() bool {
	switch r.Status {
	case StatusOK, StatusOKVia, StatusWarn:
		return false
	}
	return true
}

// String renders the result for stdout.
func (r Result) String() string {
	switch r.Status {
	case StatusOK, StatusOKVia:
		if r.Commit != "" {
			return fmt.Sprintf("  %-12s %s -> %s", r.Status, r.BeadID, r.Commit)
		}
		return fmt.Sprintf("  %-12s %s", r.Status, r.BeadID)
	case StatusWarn:
		return fmt.Sprintf("  %-12s %s — %s", r.Status, r.BeadID, r.Detail)
	case StatusMissing:
		return fmt.Sprintf("  %-12s %s — %s", r.Status, r.BeadID, r.Detail)
	case StatusFalseClosed:
		out := fmt.Sprintf("  %-12s %s — close_reason claims %s but that commit's subject does not reference %s",
			r.Status, r.BeadID, r.Commit, r.BeadID)
		if r.Subject != "" {
			out += fmt.Sprintf("\n                  subject: %s", r.Subject)
		}
		return out
	case StatusOrphan:
		return fmt.Sprintf("  %-12s %s — %s", r.Status, r.BeadID, r.Detail)
	}
	return fmt.Sprintf("  %-12s %s", r.Status, r.BeadID)
}

// Boundary regex for SubjectReferencesBead. Built per-call from the literal
// bead-id via regexp.QuoteMeta.
//
// Prefix:   start-of-line OR a non-bead-id character ([\s(\[/:]).
// Suffix:   end-of-line OR a non-bead-id character ([\s)\],:/]) OR period
//           followed by whitespace/EOL.
//
// Goal: disambiguate cp-345 from cp-345.1 while still matching natural commit
// subjects like "fix(cp-345):", "docs/cp-345/inventory", "resolve cp-345.",
// "(cp-wssr.1)". For cp-345.1, the period-then-digit is not a valid suffix
// for cp-345, so cp-345 does NOT match a subject that only contains cp-345.1.
var (
	subjectMatchPrefix = `(^|[\s(\[/:])`
	subjectMatchSuffix = `($|[\s)\],:/]|\.(\s|$))`
)

// SubjectReferencesBead returns true if `subject` mentions `beadID` as a
// standalone token. Used for the close_reason cross-check (N1) and as the
// gating predicate for `gt done` pre-CLOSE verification (N5).
//
// Token boundaries: bead-id must be preceded by start-of-line, space, '(', '[',
// '/' or ':' and followed by end-of-line, space, ')', ']', ',', ':', or '. '.
// This avoids cp-345 matching cp-345.1 while still matching natural commit subjects:
//
//	"fix(cp-345): ..."     → true
//	"fix(cp-345.1): ..."   → true (for cp-345.1)
//	"feat: cp-345 done"    → true (for cp-345)
//	"refactor cp-345.1 stuff" → false (for cp-345)
func SubjectReferencesBead(subject, beadID string) bool {
	if subject == "" || beadID == "" {
		return false
	}
	pattern := subjectMatchPrefix + regexp.QuoteMeta(beadID) + subjectMatchSuffix
	re, err := regexp.Compile(pattern)
	if err != nil {
		return false // fail-open
	}
	return re.MatchString(subject)
}

// PolecatHealth captures the four health signals for a polecat assignee.
type PolecatHealth struct {
	InRegistry   bool
	WorktreeOK   bool
	SessionOK    bool
	BondCount    int // number of HOOKED beads assigned to this polecat
	WorktreeDir  string
	SessionName  string
}

// AllOK returns true when every signal is healthy and bond count is 1 (or 0 in
// the no-double-bond direction).
func (h PolecatHealth) AllOK() bool {
	return h.InRegistry && h.WorktreeOK && h.SessionOK && h.BondCount <= 1
}

// FailureReasons returns a comma-separated list of failed checks.
func (h PolecatHealth) FailureReasons() string {
	var parts []string
	if !h.InRegistry {
		parts = append(parts, "registry-missing")
	}
	if !h.WorktreeOK {
		parts = append(parts, "worktree-missing")
	}
	if !h.SessionOK {
		parts = append(parts, "session-dead")
	}
	if h.BondCount > 1 {
		parts = append(parts, fmt.Sprintf("double-bond(%d)", h.BondCount))
	}
	return strings.Join(parts, ",")
}

// CheckPolecatHealth probes the four health signals for a given assignee.
// Fail-open: when probes can't run (no tmux, no filesystem perms, transient
// errors) we assume the signal is healthy rather than returning a false ORPHAN.
//
// rig: rig name (e.g. "CIPcodes")
// polecat: bare polecat name (e.g. "obsidian")
// registryNames: set of polecats reported by `gt polecat list <rig> --json`
// hookedAssignees: list of assignees across all HOOKED beads in the rig (for double-bond)
func CheckPolecatHealth(rig, polecat string, registryNames map[string]bool, hookedAssignees []string) PolecatHealth {
	home, _ := os.UserHomeDir()
	worktree := filepath.Join(home, "gt", rig, "polecats", polecat)
	h := PolecatHealth{
		WorktreeDir: worktree,
	}
	if _, err := os.Stat(worktree); err == nil {
		h.WorktreeOK = true
	}
	h.InRegistry = registryNames[polecat]

	// tmux session check: try both `<polecat>` and `<rig>-<polecat>` naming patterns.
	for _, name := range []string{polecat, rig + "-" + polecat} {
		if err := exec.Command("tmux", "has-session", "-t", name).Run(); err == nil {
			h.SessionOK = true
			h.SessionName = name
			break
		}
	}
	// If tmux isn't available at all, fail-open: assume session OK.
	if _, err := exec.LookPath("tmux"); err != nil {
		h.SessionOK = true
	}

	// Bond count: how many HOOKED beads in this rig point at this polecat.
	for _, a := range hookedAssignees {
		// a is a full assignee like "CIPcodes/polecats/obsidian"; tail-match polecat name
		parts := strings.Split(a, "/")
		if len(parts) > 0 && parts[len(parts)-1] == polecat {
			h.BondCount++
		}
	}
	return h
}
