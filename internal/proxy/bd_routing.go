// Prefix-aware routing for bd write commands invoked via the proxy.
//
// bd validates that --id <prefix>-XXX matches the resolved DB's allowed
// prefixes and rejects writes where the prefix doesn't match. From a crew
// workspace whose .beads/redirect points at the town (hq) DB, "bd create
// --id in-foo" fails with "prefix mismatch" even though the user clearly
// intends the in-prefixed rig DB. (gt-b6j)
//
// resolveBdCwd looks for an explicit --id flag in bd's argv and, when the
// prefix maps to a non-town rig via routes.jsonl, returns the rig directory
// to run bd from. bd's auto-resolution then lands on the correct rig .beads.
package proxy

import (
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
)

// resolveBdCwd returns a working directory for the bd subprocess when argv
// contains an explicit --id <prefix>-XXX flag whose prefix maps to a non-town
// rig in townRoot/.beads/routes.jsonl. Returns "" when no override is needed
// (not a bd command, no --id flag, unknown prefix, or town-level prefix).
func resolveBdCwd(townRoot string, argv []string) string {
	if townRoot == "" || len(argv) < 2 {
		return ""
	}
	if filepath.Base(argv[0]) != "bd" {
		return ""
	}
	id := extractIDFlag(argv[1:])
	if id == "" {
		return ""
	}
	townBeads := filepath.Join(townRoot, ".beads")
	resolved := beads.ResolveBeadsDirForID(townBeads, id)
	if resolved == "" || resolved == townBeads {
		return ""
	}
	return filepath.Dir(resolved)
}

// extractIDFlag scans args for --id=X or --id X and returns the first X found.
// Returns "" if no --id flag is present or its value is empty.
func extractIDFlag(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--id":
			if i+1 < len(args) {
				return args[i+1]
			}
			return ""
		case strings.HasPrefix(a, "--id="):
			return strings.TrimPrefix(a, "--id=")
		}
	}
	return ""
}
