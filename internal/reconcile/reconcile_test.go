package reconcile

import "testing"

func TestSubjectReferencesBead(t *testing.T) {
	cases := []struct {
		subject string
		beadID  string
		want    bool
		why     string
	}{
		// Common commit-message patterns that SHOULD match.
		{"fix(cp-345): cite cleanup", "cp-345", true, "fix(BEAD): subject"},
		{"feat(impact-methodology): cp-wssr cohort-floor patches v1.0→v1.1 (cp-wssr.1)", "cp-wssr.1", true, "trailing parenthesized bead-id"},
		{"feat(briefs): CSU memo (hq-136j)", "hq-136j", true, "parenthesized hq-id"},
		{"docs/cp-345/inventory: list NPRM cites", "cp-345", true, "slash-prefixed bead-id"},
		{"refactor: cp-345 only", "cp-345", true, "subject-trailing bead-id"},
		{"resolve cp-345.", "cp-345", true, "period-then-EOL"},
		{"fix cp-345, cp-346 dual", "cp-345", true, "comma-separated"},

		// CRITICAL: cp-345 must NOT match cp-345.1 (the false-positive that broke
		// my shell prototype's first run). When the only mention of cp-345 in the
		// subject is as a prefix of cp-345.1, the boundary check must reject it.
		{"feat: cp-345.1 partial", "cp-345", false, "cp-345.1 alone must not match cp-345"},
		{"docs(cp-345.1): scope note", "cp-345", false, "cp-345.1 in parens must not match cp-345"},

		// When BOTH cp-wssr (parent) AND cp-wssr.1 (child) appear in the subject,
		// the parent IS legitimately referenced — recency / preference logic at the
		// finder level handles the case where we want the most-specific match.
		{"feat(impact-methodology): cp-wssr cohort-floor patches v1.0→v1.1 (cp-wssr.1)", "cp-wssr", true, "cp-wssr appears as token even when cp-wssr.1 also present"},
		{"feat: cp-345abc", "cp-345", false, "cp-345abc not a token"},
		{"feat: ncp-345 typo", "cp-345", false, "preceded by alpha"},

		// CRITICAL: substring of an unrelated bead-id (cp-7g7k.7 vs "cp-7g7k.7 through .12").
		// The shell prototype's first version matched cp-7g7k.7 against an old commit
		// that listed "cp-7g7k.7 through .12" — must NOT match because the followup
		// is " through" (alpha word) — there's a space before "through" but no space
		// before that alphanumeric word.
		// Actually wait — "cp-7g7k.7 through" — bead is followed by " ", and " through"
		// IS a valid match per our boundary spec. The disambiguation here actually has
		// to come from --since (recency), not from boundary regex. Document and test.
		{"6 follow-on beads filed (cp-7g7k.7 through .12)", "cp-7g7k.7", true, "matches — recency must filter, not boundary"},
		{"6 follow-on beads filed (cp-7g7k.7 through .12)", "cp-7g7k.8", false, "cp-7g7k.8 not present at all"},

		// Edge: empty inputs.
		{"", "cp-345", false, "empty subject"},
		{"some subject", "", false, "empty bead-id"},
	}
	for _, c := range cases {
		got := SubjectReferencesBead(c.subject, c.beadID)
		if got != c.want {
			t.Errorf("SubjectReferencesBead(%q, %q) = %v, want %v (%s)", c.subject, c.beadID, got, c.want, c.why)
		}
	}
}

func TestPolecatHealth(t *testing.T) {
	t.Run("AllOK requires every signal", func(t *testing.T) {
		good := PolecatHealth{InRegistry: true, WorktreeOK: true, SessionOK: true, BondCount: 1}
		if !good.AllOK() {
			t.Errorf("expected AllOK to be true for fully-healthy polecat")
		}
	})

	t.Run("missing registry fails", func(t *testing.T) {
		h := PolecatHealth{InRegistry: false, WorktreeOK: true, SessionOK: true, BondCount: 1}
		if h.AllOK() {
			t.Errorf("expected AllOK to be false when not in registry")
		}
		if got := h.FailureReasons(); got != "registry-missing" {
			t.Errorf("FailureReasons = %q, want %q", got, "registry-missing")
		}
	})

	t.Run("double bond fails", func(t *testing.T) {
		h := PolecatHealth{InRegistry: true, WorktreeOK: true, SessionOK: true, BondCount: 2}
		if h.AllOK() {
			t.Errorf("expected AllOK to be false on double-bond")
		}
		if got := h.FailureReasons(); got != "double-bond(2)" {
			t.Errorf("FailureReasons = %q, want %q", got, "double-bond(2)")
		}
	})

	t.Run("multiple failures", func(t *testing.T) {
		h := PolecatHealth{InRegistry: false, WorktreeOK: false, SessionOK: false, BondCount: 3}
		got := h.FailureReasons()
		want := "registry-missing,worktree-missing,session-dead,double-bond(3)"
		if got != want {
			t.Errorf("FailureReasons = %q, want %q", got, want)
		}
	})

	t.Run("zero bond is ok", func(t *testing.T) {
		// BondCount=0 means the polecat exists but isn't actively bonded — that's
		// fine; AllOK should return true.
		h := PolecatHealth{InRegistry: true, WorktreeOK: true, SessionOK: true, BondCount: 0}
		if !h.AllOK() {
			t.Errorf("expected AllOK to be true at BondCount=0")
		}
	})
}

// stubFinder lets us drive VerifyClosedBead without shelling out to git.
type stubFinder struct {
	commit, subject string
	subjectErr      error
	subjectByCommit map[string]string
}

func (s stubFinder) FindCommitWithBead(_, _, _ string) (string, string, error) {
	return s.commit, s.subject, nil
}
func (s stubFinder) CommitSubject(commit string) (string, error) {
	if s.subjectErr != nil {
		return "", s.subjectErr
	}
	if s.subjectByCommit != nil {
		return s.subjectByCommit[commit], nil
	}
	return "", nil
}

// VerifyClosedBead's full path is harder to test without a stub for BeadShow.
// The most important branch — "no commit found, close_reason claims wrong sha"
// — is exercised end-to-end via the live integration. The boundary regex is
// covered by TestSubjectReferencesBead which is the actual decision predicate.
// Future work (filed as part of N5 follow-on): inject a beadShower interface
// so VerifyClosedBead can be tested in isolation.
//
// The stubFinder is kept here to scaffold that future work.
var _ CommitFinder = stubFinder{}
