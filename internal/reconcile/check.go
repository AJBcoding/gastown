package reconcile

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// commitShaPattern extracts a commit SHA from a close_reason line like:
//
//	"commit_sha: afd0e26c86bd140e03c4df0ac47891db5d4e9e44"
//	"commit_sha: afd0e26"
var commitShaPattern = regexp.MustCompile(`commit_sha:\s*([0-9a-f]{7,40})`)

// noCodePatterns identifies legitimate "closed without code changes" reasons
// (planning beads, already-fixed-elsewhere, etc.). Matching this pattern
// downgrades a MISSING result to WARN.
var noCodePatterns = regexp.MustCompile(`(?i)no.changes|no code changes|already (fixed|landed|completed)|completed with no|no-changes`)

// BdShowJSON is a minimal struct that captures the bd-show fields we use.
// bd show --json returns an array of one object; callers should unmarshal into []BdShowJSON.
type BdShowJSON struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	Assignee    string `json:"assignee"`
	ClosedAt    string `json:"closed_at"`
	CloseReason string `json:"close_reason"`
}

// PolecatJSON is the shape of `gt polecat list <rig> --json`.
type PolecatJSON struct {
	Rig            string `json:"rig"`
	Name           string `json:"name"`
	State          string `json:"state"`
	SessionRunning bool   `json:"session_running"`
}

// BeadShow shells out to `bd show <id> --json` and parses the first record.
// Returns nil + nil-error if the bead is not found (caller decides handling).
func BeadShow(beadsDir, beadID string) (*BdShowJSON, error) {
	args := []string{"show", beadID, "--json"}
	cmd := exec.Command("bd", args...)
	if beadsDir != "" {
		cmd.Env = append(cmd.Environ(), "BEADS_DIR="+beadsDir)
	}
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("bd show %s: %w", beadID, err)
	}
	var arr []BdShowJSON
	if err := json.Unmarshal(out, &arr); err != nil {
		return nil, fmt.Errorf("parse bd show %s json: %w", beadID, err)
	}
	if len(arr) == 0 {
		return nil, nil
	}
	return &arr[0], nil
}

// PolecatList shells out to `gt polecat list <rig> --json`.
func PolecatList(rig string) ([]PolecatJSON, error) {
	args := []string{"polecat", "list"}
	if rig != "" {
		args = append(args, rig)
	}
	args = append(args, "--json")
	out, err := exec.Command("gt", args...).Output()
	if err != nil {
		return nil, fmt.Errorf("gt polecat list %s: %w", rig, err)
	}
	var arr []PolecatJSON
	if err := json.Unmarshal(out, &arr); err != nil {
		return nil, fmt.Errorf("parse polecat list json: %w", err)
	}
	return arr, nil
}

// ListClosedBeads returns bead IDs closed since `since` in the working dir.
// Excludes infra types (wisp, agent, rig, role, message, gate, convoy, epic, formula).
func ListClosedBeads(workDir, since string) ([]string, error) {
	args := []string{
		"list", "--status", "closed",
		"--closed-after", since,
		"--exclude-type", "wisp,agent,rig,role,message,gate,convoy,epic,formula",
		"--flat", "-n", "0",
	}
	cmd := exec.Command("bd", args...)
	cmd.Dir = workDir
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("bd list closed: %w", err)
	}
	return parseFlatBeadIDs(string(out), "✓"), nil
}

// ListHookedBeads returns bead IDs in the HOOKED state in the working dir.
func ListHookedBeads(workDir string) ([]string, error) {
	args := []string{"list", "--status", "hooked", "--flat", "-n", "0"}
	cmd := exec.Command("bd", args...)
	cmd.Dir = workDir
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("bd list hooked: %w", err)
	}
	return parseFlatBeadIDs(string(out), "◇"), nil
}

// parseFlatBeadIDs extracts bead IDs from `bd list --flat` output. Each line
// starts with a glyph (✓ ○ ◐ ◇ ● ❄), then a space, then the bead ID, then
// fields. Lines beginning with '├──' or '└──' are tree connectors; --flat
// suppresses these but we filter defensively.
func parseFlatBeadIDs(output, expectedGlyph string) []string {
	var ids []string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "├") || strings.HasPrefix(line, "└") {
			continue
		}
		// Match: <glyph> <id> ...
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if fields[0] != expectedGlyph {
			continue
		}
		id := fields[1]
		// Skip wisp ids that occasionally slip through --exclude-type.
		if strings.Contains(id, "-wisp-") {
			continue
		}
		ids = append(ids, id)
	}
	return ids
}

// CommitFinder is the minimal git interface the verifier needs. Tests can stub.
type CommitFinder interface {
	// FindCommitWithBead returns the first commit on `branch` since `since`
	// whose subject mentions `beadID` as a standalone token. Returns "" if none.
	FindCommitWithBead(branch, since, beadID string) (commit, subject string, err error)
	// CommitSubject returns the subject of the given commit.
	CommitSubject(commit string) (string, error)
}

// VerifyClosedBead implements N1: a bd-CLOSED bead must have a commit on the
// target branch whose subject references the bead-id. If close_reason claims
// a commit_sha, the claimed commit's subject must reference the bead.
//
// Returns one of: StatusOK, StatusOKVia, StatusWarn, StatusMissing, StatusFalseClosed.
//
// The 4-hour buffer before closed_at accounts for the polecat-commits-then-closes
// sequence (commit lands first, bd close marks it).
func VerifyClosedBead(finder CommitFinder, beadsDir, branch, beadID, fallbackSince string) Result {
	bead, err := BeadShow(beadsDir, beadID)
	if err != nil || bead == nil {
		// fail-open: can't read bead state
		return Result{BeadID: beadID, Status: StatusOK, Detail: "could not read bead state"}
	}

	// Compute git --since: closed_at minus 4h, falling back to fallbackSince.
	since := fallbackSince
	if bead.ClosedAt != "" {
		if t, err := time.Parse(time.RFC3339, bead.ClosedAt); err == nil {
			since = t.Add(-4 * time.Hour).UTC().Format(time.RFC3339)
		}
	}

	// Look for any commit on branch since `since` whose subject references beadID.
	commit, subject, err := finder.FindCommitWithBead(branch, since, beadID)
	if err != nil {
		// Network / git error: fail-open.
		return Result{BeadID: beadID, Status: StatusOK, Detail: "git lookup failed: " + err.Error()}
	}
	if commit != "" {
		return Result{BeadID: beadID, Status: StatusOK, Commit: commit, Subject: subject}
	}

	// No matching commit. Inspect close_reason to distinguish FALSE_CLOSED from WARN.
	if m := commitShaPattern.FindStringSubmatch(bead.CloseReason); len(m) > 1 {
		claimedSHA := m[1]
		claimedSubject, sErr := finder.CommitSubject(claimedSHA)
		if sErr != nil {
			// Can't read the claimed commit — fail-open as MISSING (still a divergence, but lighter).
			return Result{BeadID: beadID, Status: StatusMissing,
				Detail: fmt.Sprintf("close_reason claims %s but cannot read it: %v", claimedSHA, sErr)}
		}
		if SubjectReferencesBead(claimedSubject, beadID) {
			// Edge case: --grep missed it but close_reason claim is valid.
			return Result{BeadID: beadID, Status: StatusOKVia, Commit: claimedSHA, Subject: claimedSubject}
		}
		return Result{BeadID: beadID, Status: StatusFalseClosed,
			Commit: claimedSHA, Subject: claimedSubject,
			Detail: fmt.Sprintf("close_reason claims %s but subject does not reference %s", claimedSHA, beadID)}
	}

	if noCodePatterns.MatchString(bead.CloseReason) {
		return Result{BeadID: beadID, Status: StatusWarn,
			Detail: "closed with no-code reason; no commit found (acceptable for planning beads)"}
	}

	return Result{BeadID: beadID, Status: StatusMissing,
		Detail: fmt.Sprintf("closed but no commit on %s references it (since=%s)", branch, since)}
}

// CheckHookedBead implements N4: a HOOKED bead must have a healthy assignee.
// Returns StatusOK if assignee is healthy, StatusOrphan otherwise.
//
// rig: rig name (e.g. "CIPcodes")
// hookedAssignees: list of assignees across ALL hooked beads (for double-bond detection)
// registryNames: set of polecats from `gt polecat list`
func CheckHookedBead(beadsDir, rig, beadID string, hookedAssignees []string, registryNames map[string]bool) Result {
	bead, err := BeadShow(beadsDir, beadID)
	if err != nil || bead == nil {
		return Result{BeadID: beadID, Status: StatusOK, Detail: "could not read bead state"}
	}
	if bead.Assignee == "" {
		return Result{BeadID: beadID, Status: StatusOrphan, Detail: "HOOKED but no assignee"}
	}

	parts := strings.Split(bead.Assignee, "/")
	polecat := parts[len(parts)-1]
	h := CheckPolecatHealth(rig, polecat, registryNames, hookedAssignees)
	if h.AllOK() {
		return Result{BeadID: beadID, Status: StatusOK,
			Detail: fmt.Sprintf("hooked-to %s (registry=ok worktree=ok session=ok bond=match)", bead.Assignee)}
	}
	return Result{BeadID: beadID, Status: StatusOrphan,
		Detail: fmt.Sprintf("hooked-to %s :: %s", bead.Assignee, h.FailureReasons())}
}

// HookedAssigneesAndIDs returns (ids, assignees) for HOOKED beads in workDir.
// Used to compute the double-bond denominator before per-bead health checks.
func HookedAssigneesAndIDs(beadsDir, workDir string) (ids []string, assignees []string, err error) {
	ids, err = ListHookedBeads(workDir)
	if err != nil {
		return nil, nil, err
	}
	for _, id := range ids {
		bead, e := BeadShow(beadsDir, id)
		if e != nil || bead == nil {
			continue
		}
		assignees = append(assignees, bead.Assignee)
	}
	return ids, assignees, nil
}

// PolecatRegistry returns a name-set for the rig from `gt polecat list`.
func PolecatRegistry(rig string) (map[string]bool, error) {
	pls, err := PolecatList(rig)
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(pls))
	for _, p := range pls {
		set[p.Name] = true
	}
	return set, nil
}

// _ keeps the context import live for future timeout-bounded variants.
var _ = context.Background
