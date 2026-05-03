package quota

import (
	"testing"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
)

// TestSamplePaneUsage_NilAccounts: nil accounts → empty map, no error.
func TestSamplePaneUsage_NilAccounts(t *testing.T) {
	setupTestRegistry(t)
	tmux := &mockTmux{}
	got, err := SamplePaneUsage(tmux, nil, nil)
	if err != nil {
		t.Fatalf("SamplePaneUsage: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty map, got %d entries", len(got))
	}
}

// TestSamplePaneUsage_PercentMatched: a session whose pane carries a recognized
// warning is reported with Source=pane and the parsed percent.
func TestSamplePaneUsage_PercentMatched(t *testing.T) {
	setupTestRegistry(t)
	tmux := &mockTmux{
		sessions: []string{"gt-crew-bear"},
		paneContent: map[string]string{
			"gt-crew-bear": `⏺ Working on quota probe.
You've used 87% of your weekly limit.
❯`,
		},
		envVars: map[string]map[string]string{
			"gt-crew-bear": {"CLAUDE_CONFIG_DIR": "/home/user/.claude-accounts/work"},
		},
	}
	accounts := &config.AccountsConfig{
		Accounts: map[string]config.Account{
			"work": {ConfigDir: "/home/user/.claude-accounts/work"},
		},
	}

	got, err := SamplePaneUsage(tmux, accounts, nil)
	if err != nil {
		t.Fatalf("SamplePaneUsage: %v", err)
	}
	work, ok := got["work"]
	if !ok {
		t.Fatalf("expected entry for handle 'work'; got map: %+v", got)
	}
	if work.Source != UsageSourcePane {
		t.Errorf("Source = %q, want pane", work.Source)
	}
	if work.PercentUsed != 87 {
		t.Errorf("PercentUsed = %d, want 87", work.PercentUsed)
	}
	if work.Session != "gt-crew-bear" {
		t.Errorf("Session = %q, want gt-crew-bear", work.Session)
	}
}

// TestSamplePaneUsage_RegisteredButUnmatched: account exists but no session
// pane matches → entry returned with Source=unknown, PercentUsed=0.
func TestSamplePaneUsage_RegisteredButUnmatched(t *testing.T) {
	setupTestRegistry(t)
	tmux := &mockTmux{
		sessions: []string{"gt-crew-bear"},
		paneContent: map[string]string{
			"gt-crew-bear": `⏺ Working — no warnings here.
❯`,
		},
		envVars: map[string]map[string]string{
			"gt-crew-bear": {"CLAUDE_CONFIG_DIR": "/home/user/.claude-accounts/work"},
		},
	}
	accounts := &config.AccountsConfig{
		Accounts: map[string]config.Account{
			"work":     {ConfigDir: "/home/user/.claude-accounts/work"},
			"personal": {ConfigDir: "/home/user/.claude-accounts/personal"},
		},
	}

	got, err := SamplePaneUsage(tmux, accounts, nil)
	if err != nil {
		t.Fatalf("SamplePaneUsage: %v", err)
	}
	for _, h := range []string{"work", "personal"} {
		entry, ok := got[h]
		if !ok {
			t.Errorf("missing entry for handle %q", h)
			continue
		}
		if entry.Source != UsageSourceUnknown {
			t.Errorf("%s.Source = %q, want unknown", h, entry.Source)
		}
		if entry.PercentUsed != 0 {
			t.Errorf("%s.PercentUsed = %d, want 0", h, entry.PercentUsed)
		}
	}
}

// TestSamplePaneUsage_HighestPercentWins: two sessions for the same handle,
// the closer-to-limit signal is the operationally relevant one.
func TestSamplePaneUsage_HighestPercentWins(t *testing.T) {
	setupTestRegistry(t)
	tmux := &mockTmux{
		sessions: []string{"gt-crew-bear", "gt-witness"},
		paneContent: map[string]string{
			"gt-crew-bear": `You've used 60% of your weekly limit.`,
			"gt-witness":   `You've used 92% of your weekly limit.`,
		},
		envVars: map[string]map[string]string{
			"gt-crew-bear": {"CLAUDE_CONFIG_DIR": "/home/user/.claude-accounts/work"},
			"gt-witness":   {"CLAUDE_CONFIG_DIR": "/home/user/.claude-accounts/work"},
		},
	}
	accounts := &config.AccountsConfig{
		Accounts: map[string]config.Account{
			"work": {ConfigDir: "/home/user/.claude-accounts/work"},
		},
	}

	got, err := SamplePaneUsage(tmux, accounts, nil)
	if err != nil {
		t.Fatalf("SamplePaneUsage: %v", err)
	}
	if got["work"].PercentUsed != 92 {
		t.Errorf("PercentUsed = %d, want 92 (closer-to-limit wins)", got["work"].PercentUsed)
	}
}

// TestSamplePaneUsage_CustomPatternsOverride: operator-provided patterns
// fully replace the defaults (no merge).
func TestSamplePaneUsage_CustomPatternsOverride(t *testing.T) {
	setupTestRegistry(t)
	tmux := &mockTmux{
		sessions: []string{"gt-crew-bear"},
		paneContent: map[string]string{
			"gt-crew-bear": `quota-meter: 73% used`,
		},
		envVars: map[string]map[string]string{
			"gt-crew-bear": {"CLAUDE_CONFIG_DIR": "/home/user/.claude-accounts/work"},
		},
	}
	accounts := &config.AccountsConfig{
		Accounts: map[string]config.Account{
			"work": {ConfigDir: "/home/user/.claude-accounts/work"},
		},
	}

	// Default patterns wouldn't match "quota-meter: 73% used".
	gotDefault, err := SamplePaneUsage(tmux, accounts, nil)
	if err != nil {
		t.Fatalf("SamplePaneUsage(default): %v", err)
	}
	if gotDefault["work"].Source != UsageSourceUnknown {
		t.Errorf("default patterns unexpectedly matched 'quota-meter: 73%% used': %+v", gotDefault["work"])
	}

	// Custom pattern should match.
	custom := []string{`quota-meter:\s*(\d{1,3})%`}
	gotCustom, err := SamplePaneUsage(tmux, accounts, custom)
	if err != nil {
		t.Fatalf("SamplePaneUsage(custom): %v", err)
	}
	if gotCustom["work"].PercentUsed != 73 {
		t.Errorf("custom-patterns PercentUsed = %d, want 73", gotCustom["work"].PercentUsed)
	}
}

// TestSamplePaneUsage_BadPattern: malformed pattern → error.
func TestSamplePaneUsage_BadPattern(t *testing.T) {
	setupTestRegistry(t)
	tmux := &mockTmux{}
	accounts := &config.AccountsConfig{Accounts: map[string]config.Account{}}

	_, err := SamplePaneUsage(tmux, accounts, []string{`(unclosed`})
	if err == nil {
		t.Error("expected error for malformed regex; got nil")
	}

	// No capture group → also rejected (would make percent extraction ambiguous).
	_, err = SamplePaneUsage(tmux, accounts, []string{`weekly limit reached`})
	if err == nil {
		t.Error("expected error for pattern without capture group; got nil")
	}
}

// TestDefaultWeeklyUsagePatterns_ProvisionalSetMatches exercises every
// default pattern against a representative warning string. Failure here
// means the default set has lost a known-good case — investigate before
// shipping a release.
//
// CALIBRATION CAVEAT: these strings are model fixtures, NOT confirmed
// captures from current Claude Code. The N6 dry-run (hq-2h2d) is the
// only thing that will tell us if the *real* warnings match.
func TestDefaultWeeklyUsagePatterns_ProvisionalSetMatches(t *testing.T) {
	cases := []struct {
		name string
		line string
		want int
	}{
		{"contraction-form", "You've used 87% of your weekly limit", 87},
		{"plain-form", "87% of weekly usage", 87},
		{"colon-form", "weekly usage: 92%", 92},
		{"approaching-form", "Approaching weekly limit (78%)", 78},
	}
	compiled, err := compileUsagePatterns(constants.DefaultWeeklyUsagePatterns)
	if err != nil {
		t.Fatalf("compileUsagePatterns(defaults): %v", err)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _, ok := matchUsagePercent(tc.line+"\n", compiled)
			if !ok {
				t.Fatalf("no default pattern matched %q", tc.line)
			}
			if got != tc.want {
				t.Errorf("matched percent = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestMatchUsagePercent_RejectsImplausibleNumbers: a percent capture > 100 is
// almost certainly the wrong number — fall through to the next pattern.
func TestMatchUsagePercent_RejectsImplausibleNumbers(t *testing.T) {
	patterns := []string{`(\d+)% of weekly usage`}
	compiled, err := compileUsagePatterns(patterns)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if _, _, ok := matchUsagePercent("250% of weekly usage", compiled); ok {
		t.Error("expected no match for 250% (implausible)")
	}
	if got, _, ok := matchUsagePercent("80% of weekly usage", compiled); !ok || got != 80 {
		t.Errorf("expected match=80, got match=%v percent=%d", ok, got)
	}
}
