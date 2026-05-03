package quota

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
)

// UsageSource indicates how an AccountUsage value was obtained.
type UsageSource string

const (
	// UsageSourcePane means percent-used was extracted from a tmux pane match.
	UsageSourcePane UsageSource = "pane"
	// UsageSourceUnknown means no signal was obtained (no pattern matched, or
	// no Gas Town session was found for this account). PercentUsed will be 0;
	// callers must NOT treat it as "0% used".
	UsageSourceUnknown UsageSource = "unknown"
)

// AccountUsage is the usage signal for one Claude Code account, derived from
// scanning that account's tmux session(s) for Claude's near-limit warning text.
//
// Independent of WeeklyTokenBudget (hq-ktzz / N1): this struct reports a percent
// observed in the TUI, not absolute tokens — the soft/hard threshold logic
// (hq-aten / N4) compares PercentUsed against thresholds directly.
type AccountUsage struct {
	Handle      string      // account handle (may be "" if config_dir didn't resolve)
	Session     string      // tmux session that yielded the signal (last-wins if multiple)
	ConfigDir   string      // CLAUDE_CONFIG_DIR for the session
	PercentUsed int         // weekly percent used (0 when Source=unknown)
	Source      UsageSource // "pane" when percent extracted from a pattern match; "unknown" otherwise
	MatchedLine string      // the pane line that matched (debugging / N6 calibration)
}

// SamplePaneUsage scans Gas Town tmux sessions for weekly-quota warning text
// and returns a per-account-handle map of usage signals.
//
// Behavior:
//   - One AccountUsage entry per resolved handle. Sessions whose CLAUDE_CONFIG_DIR
//     does not match a registered account are skipped (no anonymous bucket).
//   - When a session matches a pattern, the caller's map for that handle gets
//     PercentUsed populated and Source=UsageSourcePane.
//   - When NO session for a handle yields a match, the handle is still represented
//     in the map (so callers can see "we have an account but no usable signal")
//     with PercentUsed=0 and Source=UsageSourceUnknown.
//   - If multiple sessions match for the same handle, the highest PercentUsed wins
//     (closest-to-limit signal is the operationally relevant one).
//
// patterns may be nil/empty — in that case constants.DefaultWeeklyUsagePatterns
// is used. accounts must not be nil; if it is, returns an empty map and no error.
//
// CALIBRATION NOTE: see the package doc on DefaultWeeklyUsagePatterns. Until N6
// (hq-2h2d) confirms the regexes against current Claude Code output, callers
// should treat Source=UsageSourceUnknown as the common case and prefer the
// transcript-based probe (N3 / SampleTranscriptUsage) when accuracy matters.
func SamplePaneUsage(tmux TmuxClient, accounts *config.AccountsConfig, patterns []string) (map[string]AccountUsage, error) {
	if accounts == nil {
		return map[string]AccountUsage{}, nil
	}

	if len(patterns) == 0 {
		patterns = constants.DefaultWeeklyUsagePatterns
	}
	compiled, err := compileUsagePatterns(patterns)
	if err != nil {
		return nil, err
	}

	sessions, err := tmux.ListSessions()
	if err != nil {
		return nil, fmt.Errorf("listing sessions: %w", err)
	}

	// Pre-seed the result with every registered handle as "unknown" so the
	// caller sees the full account list even when no session yields a signal.
	result := make(map[string]AccountUsage, len(accounts.Accounts))
	for handle := range accounts.Accounts {
		result[handle] = AccountUsage{Handle: handle, Source: UsageSourceUnknown}
	}

	// Reuse Scanner's account-resolution machinery to keep the resolution rule
	// in one place (CLAUDE_CONFIG_DIR + GT_QUOTA_ACCOUNT override). We don't
	// run the rate-limit scan — just borrow the resolver.
	resolver := &Scanner{tmux: tmux, accounts: accounts}

	for _, sess := range sessions {
		if !isGasTownSession(sess) {
			continue
		}
		handle := resolver.resolveAccountHandle(sess)
		if handle == "" {
			continue // unmapped session
		}
		configDir := ""
		if cd, err := tmux.GetEnvironment(sess, "CLAUDE_CONFIG_DIR"); err == nil {
			configDir = strings.TrimSpace(cd)
		} else if home, hErr := os.UserHomeDir(); hErr == nil {
			configDir = home + "/.claude"
		}

		content, err := tmux.CapturePane(sess, scanLines)
		if err != nil {
			continue // dead session
		}

		percent, line, ok := matchUsagePercent(content, compiled)
		if !ok {
			continue
		}

		// Closest-to-limit wins when multiple sessions report for one handle.
		if existing, found := result[handle]; found && existing.Source == UsageSourcePane && existing.PercentUsed >= percent {
			continue
		}
		result[handle] = AccountUsage{
			Handle:      handle,
			Session:     sess,
			ConfigDir:   configDir,
			PercentUsed: percent,
			Source:      UsageSourcePane,
			MatchedLine: line,
		}
	}

	return result, nil
}

// compileUsagePatterns compiles the input patterns and rejects any without
// exactly one numeric capture group (which would make percent extraction
// ambiguous or impossible).
func compileUsagePatterns(patterns []string) ([]*regexp.Regexp, error) {
	out := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("compiling usage pattern %q: %w", p, err)
		}
		// NumSubexp counts capture groups; require >= 1 (extra groups are
		// allowed for context, but at least one is needed for the percent).
		if re.NumSubexp() < 1 {
			return nil, fmt.Errorf("usage pattern %q has no capture group; need one for percent integer", p)
		}
		out = append(out, re)
	}
	return out, nil
}

// matchUsagePercent walks pane content bottom-up and returns the first
// successful percent extraction (a capture group that parses as 0-100).
// Walks bottom-up because the most recent warning is what we want to act on.
func matchUsagePercent(content string, patterns []*regexp.Regexp) (percent int, line string, ok bool) {
	allLines := strings.Split(content, "\n")
	start := len(allLines) - checkLines
	if start < 0 {
		start = 0
	}
	bottom := allLines[start:]
	for i := len(bottom) - 1; i >= 0; i-- {
		ln := strings.TrimSpace(bottom[i])
		if ln == "" {
			continue
		}
		for _, re := range patterns {
			m := re.FindStringSubmatch(ln)
			if len(m) < 2 {
				continue
			}
			// First capture group is the percent integer.
			n, err := strconv.Atoi(m[1])
			if err != nil {
				continue
			}
			if n < 0 || n > 100 {
				continue // implausible — likely matched the wrong number
			}
			return n, ln, true
		}
	}
	return 0, "", false
}
