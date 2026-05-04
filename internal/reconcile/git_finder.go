package reconcile

import (
	"fmt"
	"os/exec"
	"strings"
)

// GitCmdFinder satisfies CommitFinder by shelling out to git directly.
// We don't use internal/git because that requires a *Git struct bound to a
// working directory — for the reconciler we want a self-contained finder
// scoped to repoDir. Keeping it minimal avoids pulling in the wider Git
// dependency surface for what is essentially three git invocations.
type GitCmdFinder struct {
	RepoDir string
}

// FindCommitWithBead scans up to 50 candidate commits matching `--grep beadID`
// since `since`, then awk-tightens the boundary to disambiguate cp-345 from
// cp-345.1.
func (f GitCmdFinder) FindCommitWithBead(branch, since, beadID string) (string, string, error) {
	args := []string{
		"-C", f.RepoDir,
		"log", branch,
		"--since", since,
		"--grep", beadID,
		"--extended-regexp",
		"-50",
		"--format=%H %s",
	}
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		// Distinguish "no commits" from real error: git log returns 0 even with no matches.
		// An exec error here is a real problem; surface it.
		return "", "", fmt.Errorf("git log: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// First word is the SHA, rest is the subject.
		fields := strings.SplitN(line, " ", 2)
		if len(fields) < 2 {
			continue
		}
		commit, subject := fields[0], fields[1]
		if SubjectReferencesBead(subject, beadID) {
			return commit, subject, nil
		}
	}
	return "", "", nil
}

// CommitSubject returns the subject of `commit` from RepoDir.
func (f GitCmdFinder) CommitSubject(commit string) (string, error) {
	out, err := exec.Command("git", "-C", f.RepoDir, "log", "-1", "--format=%s", commit).Output()
	if err != nil {
		return "", fmt.Errorf("git log -1 %s: %w", commit, err)
	}
	return strings.TrimSpace(string(out)), nil
}
