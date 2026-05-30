package doltserver

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestBuildWispDependenciesCopyQueryUsesSplitTargetSchema(t *testing.T) {
	query := buildWispDependenciesCopyQuery(true, false)

	if !strings.Contains(query, "COALESCE(d.depends_on_issue_id, d.depends_on_wisp_id, d.depends_on_external)") {
		t.Fatalf("split-schema query did not use dependency target COALESCE: %s", query)
	}
	if strings.Contains(query, "d.depends_on_id") {
		t.Fatalf("split-schema query still selects legacy target column: %s", query)
	}
}

func TestBuildWispDependenciesCopyQueryUsesLegacyTargetSchema(t *testing.T) {
	query := buildWispDependenciesCopyQuery(false, false)

	if !strings.Contains(query, "SELECT d.issue_id, d.depends_on_id, d.type") {
		t.Fatalf("legacy query did not select depends_on_id: %s", query)
	}
}

func TestBuildWispDependenciesCopyQueryUsesSplitWispTargetSchema(t *testing.T) {
	query := buildWispDependenciesCopyQuery(false, true)

	if !strings.Contains(query, "INSERT IGNORE INTO wisp_dependencies (issue_id, depends_on_issue_id, depends_on_wisp_id, depends_on_external, type") {
		t.Fatalf("split wisp-target query did not insert typed target columns: %s", query)
	}
	if strings.Contains(query, "INSERT IGNORE INTO wisp_dependencies (issue_id, depends_on_id") {
		t.Fatalf("split wisp-target query still inserts generated depends_on_id: %s", query)
	}
	if !strings.Contains(query, "EXISTS (SELECT 1 FROM wisps target_wisp WHERE target_wisp.id = d.depends_on_id)") {
		t.Fatalf("split wisp-target query did not classify legacy targets against wisps: %s", query)
	}
}

func TestBuildWispDependenciesCopyQueryReclassifiesSplitIssueTargetsForWisps(t *testing.T) {
	query := buildWispDependenciesCopyQuery(true, true)

	if !strings.Contains(query, "EXISTS (SELECT 1 FROM wisps target_wisp WHERE target_wisp.id = d.depends_on_issue_id)") {
		t.Fatalf("split-source query did not reclassify issue targets already present in wisps: %s", query)
	}
	if !strings.Contains(query, "WHEN d.depends_on_issue_id IS NOT NULL AND (") {
		t.Fatalf("split-source query did not route moved issue targets into depends_on_wisp_id: %s", query)
	}
	if strings.Contains(query, "SELECT d.issue_id, d.depends_on_issue_id, d.depends_on_wisp_id, d.depends_on_external") {
		t.Fatalf("split-source wisp copy should not blindly copy typed target columns: %s", query)
	}
}

func TestBuildWispDependenciesCopyQueryCopiesIntoSplitGeneratedTarget(t *testing.T) {
	sqlite, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 not found")
	}

	query := strings.ReplaceAll(buildWispDependenciesCopyQuery(false, true), "INSERT IGNORE", "INSERT OR IGNORE")
	dbPath := filepath.Join(t.TempDir(), "split-wisp-dependencies.db")
	script := `
CREATE TABLE issues (
  id varchar(255) PRIMARY KEY
);
CREATE TABLE wisps (
  id varchar(255) PRIMARY KEY
);
CREATE TABLE dependencies (
  issue_id varchar(255) NOT NULL,
  depends_on_id varchar(255) NOT NULL,
  type varchar(32) NOT NULL DEFAULT 'blocks',
  created_at datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  created_by varchar(255) NOT NULL DEFAULT '',
  metadata text,
  thread_id varchar(255) DEFAULT ''
);
CREATE TABLE wisp_dependencies (
  issue_id varchar(255) NOT NULL,
  depends_on_issue_id varchar(255),
  depends_on_wisp_id varchar(255),
  depends_on_external varchar(255),
  depends_on_id varchar(255) GENERATED ALWAYS AS (COALESCE(depends_on_issue_id, depends_on_wisp_id, depends_on_external)) STORED,
  type varchar(32) NOT NULL DEFAULT 'blocks',
  created_at datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  created_by varchar(255) NOT NULL DEFAULT '',
  metadata text,
  thread_id varchar(255) DEFAULT '',
  UNIQUE (issue_id, depends_on_id),
  CHECK ((depends_on_issue_id IS NOT NULL) + (depends_on_wisp_id IS NOT NULL) + (depends_on_external IS NOT NULL) = 1)
);
INSERT INTO wisps (id) VALUES ('gt-agent'), ('gt-wisp-target');
INSERT INTO issues (id) VALUES ('gt-issue-target');
INSERT INTO dependencies (issue_id, depends_on_id, type, created_by, metadata, thread_id) VALUES
  ('gt-agent', 'gt-issue-target', 'blocks', '', '{}', ''),
  ('gt-agent', 'gt-wisp-target', 'blocks', '', '{}', ''),
  ('gt-agent', 'external:ticket-1', 'blocks', '', '{}', ''),
  ('gt-agent', 'gt-missing-target', 'blocks', '', '{}', '');
` + query + `;
.mode list
SELECT depends_on_id || '|' ||
       COALESCE(depends_on_issue_id, '') || '|' ||
       COALESCE(depends_on_wisp_id, '') || '|' ||
       COALESCE(depends_on_external, '')
FROM wisp_dependencies
ORDER BY depends_on_id;
`

	cmd := exec.Command(sqlite, "-batch", dbPath)
	cmd.Stdin = strings.NewReader(script)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sqlite fixture failed: %v\n%s", err, output)
	}

	got := strings.TrimSpace(string(output))
	want := strings.Join([]string{
		"external:ticket-1|||external:ticket-1",
		"gt-issue-target|gt-issue-target||",
		"gt-missing-target|||gt-missing-target",
		"gt-wisp-target||gt-wisp-target|",
	}, "\n")
	if got != want {
		t.Fatalf("split generated target copy rows:\n%s\nwant:\n%s", got, want)
	}
}

func TestBuildWispDependenciesCopyQueryReclassifiesSplitSourceIntoSplitWispTarget(t *testing.T) {
	sqlite, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 not found")
	}

	query := strings.ReplaceAll(buildWispDependenciesCopyQuery(true, true), "INSERT IGNORE", "INSERT OR IGNORE")
	dbPath := filepath.Join(t.TempDir(), "split-source-wisp-dependencies.db")
	script := `
CREATE TABLE issues (
  id varchar(255) PRIMARY KEY
);
CREATE TABLE wisps (
  id varchar(255) PRIMARY KEY
);
CREATE TABLE dependencies (
  issue_id varchar(255) NOT NULL,
  depends_on_issue_id varchar(255),
  depends_on_wisp_id varchar(255),
  depends_on_external varchar(255),
  type varchar(32) NOT NULL DEFAULT 'blocks',
  created_at datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  created_by varchar(255) NOT NULL DEFAULT '',
  metadata text,
  thread_id varchar(255) DEFAULT '',
  CHECK ((depends_on_issue_id IS NOT NULL) + (depends_on_wisp_id IS NOT NULL) + (depends_on_external IS NOT NULL) = 1)
);
CREATE TABLE wisp_dependencies (
  issue_id varchar(255) NOT NULL,
  depends_on_issue_id varchar(255),
  depends_on_wisp_id varchar(255),
  depends_on_external varchar(255),
  depends_on_id varchar(255) GENERATED ALWAYS AS (COALESCE(depends_on_issue_id, depends_on_wisp_id, depends_on_external)) STORED,
  type varchar(32) NOT NULL DEFAULT 'blocks',
  created_at datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
  created_by varchar(255) NOT NULL DEFAULT '',
  metadata text,
  thread_id varchar(255) DEFAULT '',
  UNIQUE (issue_id, depends_on_id),
  CHECK ((depends_on_issue_id IS NOT NULL) + (depends_on_wisp_id IS NOT NULL) + (depends_on_external IS NOT NULL) = 1)
);
INSERT INTO wisps (id) VALUES ('gt-agent'), ('gt-moving-target'), ('gt-wisp-target');
INSERT INTO issues (id) VALUES ('gt-moving-target'), ('gt-live-issue');
INSERT INTO dependencies (issue_id, depends_on_issue_id, depends_on_wisp_id, depends_on_external, type, created_by, metadata, thread_id) VALUES
  ('gt-agent', 'gt-moving-target', NULL, NULL, 'blocks', '', '{}', ''),
  ('gt-agent', 'gt-live-issue', NULL, NULL, 'blocks', '', '{}', ''),
  ('gt-agent', NULL, 'gt-wisp-target', NULL, 'blocks', '', '{}', ''),
  ('gt-agent', NULL, NULL, 'external:ticket-1', 'blocks', '', '{}', ''),
  ('gt-agent', 'gt-missing-target', NULL, NULL, 'blocks', '', '{}', '');
` + query + `;
.mode list
SELECT depends_on_id || '|' ||
       COALESCE(depends_on_issue_id, '') || '|' ||
       COALESCE(depends_on_wisp_id, '') || '|' ||
       COALESCE(depends_on_external, '')
FROM wisp_dependencies
ORDER BY depends_on_id;
`

	cmd := exec.Command(sqlite, "-batch", dbPath)
	cmd.Stdin = strings.NewReader(script)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sqlite fixture failed: %v\n%s", err, output)
	}

	got := strings.TrimSpace(string(output))
	want := strings.Join([]string{
		"external:ticket-1|||external:ticket-1",
		"gt-live-issue|gt-live-issue||",
		"gt-missing-target|||gt-missing-target",
		"gt-moving-target||gt-moving-target|",
		"gt-wisp-target||gt-wisp-target|",
	}, "\n")
	if got != want {
		t.Fatalf("split-source wisp copy rows:\n%s\nwant:\n%s", got, want)
	}
}

func TestCopyWispDependenciesRetriesSplitSourceThenGeneratedTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake bd stub is shell-specific")
	}

	logPath := installFakeBDForDependencyCopy(t)

	if err := copyWispDependencies(t.TempDir()); err != nil {
		t.Fatalf("copyWispDependencies() error = %v", err)
	}

	lines := readDependencyCopyLog(t, logPath)
	if len(lines) != 4 {
		t.Fatalf("bd queries = %v, want column probe plus 3 copy attempts", lines)
	}
	assertDependencyCopySequence(t, lines, "INNER JOIN wisps w ON d.issue_id = w.id")
}

func installFakeBDForDependencyCopy(t *testing.T) string {
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

func readDependencyCopyLog(t *testing.T, logPath string) []string {
	t.Helper()

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read fake bd log: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func assertDependencyCopySequence(t *testing.T, lines []string, finalWhere string) {
	t.Helper()

	if !strings.Contains(lines[0], "INFORMATION_SCHEMA.COLUMNS") || !strings.Contains(lines[0], "wisp_dependencies") {
		t.Fatalf("first query = %q, want wisp dependency target column probe", lines[0])
	}
	if !strings.Contains(lines[1], "INSERT IGNORE INTO wisp_dependencies (issue_id, depends_on_id") ||
		!strings.Contains(lines[1], "SELECT d.issue_id, d.depends_on_id") {
		t.Fatalf("first copy attempt = %q, want legacy source into legacy target", lines[1])
	}
	if !strings.Contains(lines[2], "INSERT IGNORE INTO wisp_dependencies (issue_id, depends_on_id") ||
		!strings.Contains(lines[2], "COALESCE(d.depends_on_issue_id, d.depends_on_wisp_id, d.depends_on_external)") {
		t.Fatalf("second copy attempt = %q, want split-source retry into legacy target", lines[2])
	}
	if !strings.Contains(lines[3], "INSERT IGNORE INTO wisp_dependencies (issue_id, depends_on_issue_id, depends_on_wisp_id, depends_on_external") ||
		!strings.Contains(lines[3], "d.depends_on_issue_id") ||
		!strings.Contains(lines[3], finalWhere) {
		t.Fatalf("third copy attempt = %q, want generated-target retry into split target columns", lines[3])
	}
}
