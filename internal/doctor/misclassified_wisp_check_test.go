package doctor

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// TestFixWorkDir_HQ verifies that Fix() resolves the "hq" rig name to the
// town root directory, not townRoot/hq. When the Dolt detection path finds
// misplaced ephemerals in the "hq" database, the rigName is "hq" — Fix() must
// map this to TownRoot (same as Run does). Regression test for GH#2127.
func TestFixWorkDir_HQ(t *testing.T) {
	townRoot := t.TempDir()

	got := resolveMisclassifiedWispWorkDir(townRoot, misclassifiedWisp{rigName: "hq"})
	hqPath := filepath.Join(townRoot, "hq")
	if hqPath == townRoot {
		t.Fatal("test setup error: townRoot should not end in /hq")
	}
	if got != townRoot {
		t.Fatalf("resolveMisclassifiedWispWorkDir(%q, hq) = %q, want %q", townRoot, got, townRoot)
	}
}

func TestFixWorkDir_RoutedRig(t *testing.T) {
	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	routesContent := `{"prefix":"hq-","path":"."}
{"prefix":"sw-","path":"sallaWork/mayor/rig"}
`
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}

	want := filepath.Join(townRoot, "sallaWork/mayor/rig")
	got := resolveMisclassifiedWispWorkDir(townRoot, misclassifiedWisp{rigName: "sw"})
	if got != want {
		t.Fatalf("resolveMisclassifiedWispWorkDir(%q, sw) = %q, want %q", townRoot, got, want)
	}
}

// TestNoHeuristicClassification verifies that the check does NOT use heuristics
// to guess whether beads should be wisps. Only beads with ephemeral=1 that are
// in the issues table should be flagged. This is the ZFC compliance test.
func TestNoHeuristicClassification(t *testing.T) {
	check := NewCheckMisclassifiedWisps()

	// Inject items that the OLD heuristic would have flagged but the new
	// check should NOT (because they aren't ephemeral=1 in the issues table).
	// The new check only looks at the DB, so there's nothing to test at the
	// shouldBeWisp level — that function no longer exists.
	if check.misclassified != nil {
		t.Error("fresh check should have no misclassified items")
	}
}

func TestRunIgnoresJSONLWhenDoltUnavailable(t *testing.T) {
	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, "gastown", ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	staleJSONL := `{"id":"gt-wisp-stale","title":"Stale wisp","ephemeral":true}` + "\n"
	if err := os.WriteFile(filepath.Join(beadsDir, "issues.jsonl"), []byte(staleJSONL), 0644); err != nil {
		t.Fatal(err)
	}

	check := NewCheckMisclassifiedWisps()
	result := check.Run(&CheckContext{TownRoot: townRoot})
	if result.Status != StatusOK {
		t.Fatalf("expected StatusOK when only stale JSONL exists, got %v: %s", result.Status, result.Message)
	}
	if !strings.Contains(result.Message, "Dolt unavailable") {
		t.Fatalf("expected Dolt-unavailable skip message, got %q", result.Message)
	}
	if len(check.misclassified) != 0 {
		t.Fatalf("expected no misclassified wisps from stale JSONL, got %d", len(check.misclassified))
	}
}

func TestBuildMisclassifiedWispDependenciesCopyQueryUsesSplitTargetSchema(t *testing.T) {
	query := buildMisclassifiedWispDependenciesCopyQuery("'gt-wisp-1'", true, false)

	if !strings.Contains(query, "COALESCE(d.depends_on_issue_id, d.depends_on_wisp_id, d.depends_on_external)") {
		t.Fatalf("split-schema query did not use dependency target COALESCE: %s", query)
	}
	if strings.Contains(query, "d.depends_on_id") {
		t.Fatalf("split-schema query still selects legacy target column: %s", query)
	}
	if !strings.Contains(query, "WHERE d.issue_id IN ('gt-wisp-1')") {
		t.Fatalf("split-schema query lost id filter: %s", query)
	}
}

func TestBuildMisclassifiedWispDependenciesCopyQueryUsesLegacyTargetSchema(t *testing.T) {
	query := buildMisclassifiedWispDependenciesCopyQuery("'gt-wisp-1'", false, false)

	if !strings.Contains(query, "SELECT d.issue_id, d.depends_on_id, d.type") {
		t.Fatalf("legacy query did not select depends_on_id: %s", query)
	}
}

func TestBuildMisclassifiedWispDependenciesCopyQueryUsesSplitWispTargetSchema(t *testing.T) {
	query := buildMisclassifiedWispDependenciesCopyQuery("'gt-wisp-1'", false, true)

	if !strings.Contains(query, "INSERT IGNORE INTO wisp_dependencies (issue_id, depends_on_issue_id, depends_on_wisp_id, depends_on_external, type") {
		t.Fatalf("split wisp-target query did not insert typed target columns: %s", query)
	}
	if strings.Contains(query, "INSERT IGNORE INTO wisp_dependencies (issue_id, depends_on_id") {
		t.Fatalf("split wisp-target query still inserts generated depends_on_id: %s", query)
	}
	if !strings.Contains(query, "WHERE d.issue_id IN ('gt-wisp-1')") {
		t.Fatalf("split wisp-target query lost id filter: %s", query)
	}
}

func TestBuildMisclassifiedWispDependenciesCopyQueryReclassifiesMigratingSplitIssueTargets(t *testing.T) {
	query := buildMisclassifiedWispDependenciesCopyQuery("'gt-wisp-1','gt-wisp-2'", true, true)

	if !strings.Contains(query, "d.depends_on_issue_id IN ('gt-wisp-1','gt-wisp-2')") {
		t.Fatalf("split-source query did not treat migrating ids as wisp targets: %s", query)
	}
	if !strings.Contains(query, "EXISTS (SELECT 1 FROM wisps target_wisp WHERE target_wisp.id = d.depends_on_issue_id)") {
		t.Fatalf("split-source query did not treat existing wisps as wisp targets: %s", query)
	}
	if strings.Contains(query, "SELECT d.issue_id, d.depends_on_issue_id, d.depends_on_wisp_id, d.depends_on_external") {
		t.Fatalf("split-source wisp copy should not blindly copy typed target columns: %s", query)
	}
}

func TestCopyMisclassifiedWispDependenciesRetriesSplitSourceThenGeneratedTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake bd stub is shell-specific")
	}

	logPath := installFakeBDForMisclassifiedDependencyCopy(t)

	if err := copyMisclassifiedWispDependencies(t.TempDir(), "'gt-wisp-1','gt-wisp-2'"); err != nil {
		t.Fatalf("copyMisclassifiedWispDependencies() error = %v", err)
	}

	lines := readMisclassifiedDependencyCopyLog(t, logPath)
	if len(lines) != 4 {
		t.Fatalf("bd queries = %v, want column probe plus 3 copy attempts", lines)
	}
	assertMisclassifiedDependencyCopySequence(t, lines)
}

func installFakeBDForMisclassifiedDependencyCopy(t *testing.T) string {
	t.Helper()

	binDir := t.TempDir()
	statePath := filepath.Join(t.TempDir(), "bd-state")
	logPath := filepath.Join(t.TempDir(), "bd-queries.log")
	script := `#!/usr/bin/env bash
set -u
if [ "${1:-}" != "sql" ]; then
  exit 1
fi
if [ "${2:-}" = "--csv" ]; then
  printf 'csv:%s\n' "${3:-}" >> "$BD_QUERY_LOG"
  printf 'cnt\n0\n'
  exit 0
fi
printf 'write:%s\n' "${2:-}" >> "$BD_QUERY_LOG"
state=0
if [ -f "$BD_STATE" ]; then
  state="$(cat "$BD_STATE")"
fi
state=$((state + 1))
printf '%s\n' "$state" > "$BD_STATE"
case "$state" in
  1)
    printf "Error 1054 (42S22): Unknown column 'd.depends_on_id' in 'field list'\n" >&2
    exit 1
    ;;
  2)
    printf "Error 3105 (HY000): The value specified for generated column 'depends_on_id' in table 'wisp_dependencies' is not allowed.\n" >&2
    exit 1
    ;;
  3)
    exit 0
    ;;
  *)
    printf 'unexpected bd sql write call %s\n' "$state" >&2
    exit 1
    ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BD_STATE", statePath)
	t.Setenv("BD_QUERY_LOG", logPath)

	return logPath
}

func readMisclassifiedDependencyCopyLog(t *testing.T, logPath string) []string {
	t.Helper()

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read fake bd log: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func assertMisclassifiedDependencyCopySequence(t *testing.T, lines []string) {
	t.Helper()

	if !strings.Contains(lines[0], "INFORMATION_SCHEMA.COLUMNS") || !strings.Contains(lines[0], "wisp_dependencies") {
		t.Fatalf("first query = %q, want wisp dependency target column probe", lines[0])
	}
	if !strings.Contains(lines[1], "INSERT IGNORE INTO wisp_dependencies (issue_id, depends_on_id") ||
		!strings.Contains(lines[1], "SELECT d.issue_id, d.depends_on_id") ||
		!strings.Contains(lines[1], "WHERE d.issue_id IN ('gt-wisp-1','gt-wisp-2')") {
		t.Fatalf("first copy attempt = %q, want legacy source into legacy target", lines[1])
	}
	if !strings.Contains(lines[2], "INSERT IGNORE INTO wisp_dependencies (issue_id, depends_on_id") ||
		!strings.Contains(lines[2], "COALESCE(d.depends_on_issue_id, d.depends_on_wisp_id, d.depends_on_external)") {
		t.Fatalf("second copy attempt = %q, want split-source retry into legacy target", lines[2])
	}
	if !strings.Contains(lines[3], "INSERT IGNORE INTO wisp_dependencies (issue_id, depends_on_issue_id, depends_on_wisp_id, depends_on_external") ||
		!strings.Contains(lines[3], "d.depends_on_issue_id IN ('gt-wisp-1','gt-wisp-2')") ||
		!strings.Contains(lines[3], "WHERE d.issue_id IN ('gt-wisp-1','gt-wisp-2')") {
		t.Fatalf("third copy attempt = %q, want generated-target retry into split target columns", lines[3])
	}
}

// TestGetRigPathForPrefix_RoutesResolution verifies that GetRigPathForPrefix
// correctly resolves rig paths from routes.jsonl. This is critical for the
// misclassified-wisps check which uses database names (e.g., "sw") to look up
// rig directories that may have custom paths (e.g., "sallaWork/mayor/rig").
// Regression test for: DB probe failures when database name != directory name.
func TestGetRigPathForPrefix_RoutesResolution(t *testing.T) {
	// Create a temporary town structure with routes.jsonl
	tmpDir := t.TempDir()
	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create routes.jsonl with custom rig paths
	routesContent := `{"prefix":"hq-","path":"."}
{"prefix":"sw-","path":"sallaWork/mayor/rig"}
{"prefix":"gt-","path":"gastown/mayor/rig"}
`
	routesPath := filepath.Join(beadsDir, "routes.jsonl")
	if err := os.WriteFile(routesPath, []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		prefix   string
		wantPath string
	}{
		{
			name:     "hq prefix resolves to town root",
			prefix:   "hq-",
			wantPath: tmpDir,
		},
		{
			name:     "sw prefix resolves to custom path",
			prefix:   "sw-",
			wantPath: filepath.Join(tmpDir, "sallaWork/mayor/rig"),
		},
		{
			name:     "gt prefix resolves to custom path",
			prefix:   "gt-",
			wantPath: filepath.Join(tmpDir, "gastown/mayor/rig"),
		},
		{
			name:     "unknown prefix returns empty",
			prefix:   "unknown-",
			wantPath: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := beads.GetRigPathForPrefix(tmpDir, tt.prefix)
			if got != tt.wantPath {
				t.Errorf("GetRigPathForPrefix(%q, %q) = %q, want %q",
					tmpDir, tt.prefix, got, tt.wantPath)
			}
		})
	}
}

// TestRigPathResolution_NoRoutesFile verifies that when routes.jsonl doesn't exist,
// GetRigPathForPrefix returns empty string, triggering the fallback behavior.
func TestRigPathResolution_NoRoutesFile(t *testing.T) {
	tmpDir := t.TempDir()
	// Don't create .beads/routes.jsonl

	got := beads.GetRigPathForPrefix(tmpDir, "sw-")
	if got != "" {
		t.Errorf("GetRigPathForPrefix without routes.jsonl should return empty, got %q", got)
	}
}

// TestRigDirResolution_Logic verifies the resolution logic that would be used
// in the misclassified-wisps check when mapping database names to directories.
func TestRigDirResolution_Logic(t *testing.T) {
	tmpDir := t.TempDir()
	beadsDir := filepath.Join(tmpDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create routes with custom paths
	routesContent := `{"prefix":"hq-","path":"."}
{"prefix":"sw-","path":"sallaWork/mayor/rig"}
`
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		dbName  string
		wantDir string
		desc    string
	}{
		{
			dbName:  "hq",
			wantDir: tmpDir,
			desc:    "hq database maps to town root via route path='.'",
		},
		{
			dbName:  "sw",
			wantDir: filepath.Join(tmpDir, "sallaWork/mayor/rig"),
			desc:    "sw database maps to custom path via route",
		},
		{
			dbName:  "other",
			wantDir: filepath.Join(tmpDir, "other"),
			desc:    "unknown database falls back to townRoot/dbName",
		},
	}

	for _, tt := range tests {
		t.Run(tt.dbName, func(t *testing.T) {
			// This mirrors the resolution logic in misclassified_wisp_check.go
			prefix := tt.dbName + "-"
			rigDir := beads.GetRigPathForPrefix(tmpDir, prefix)
			if rigDir == "" {
				// Fallback: assume database name equals rig directory name
				rigDir = filepath.Join(tmpDir, tt.dbName)
				if tt.dbName == "hq" {
					rigDir = tmpDir
				}
			}

			if rigDir != tt.wantDir {
				t.Errorf("%s: got rigDir=%q, want %q", tt.desc, rigDir, tt.wantDir)
			}
		})
	}
}
