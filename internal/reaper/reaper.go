// Package reaper provides wisp and issue cleanup operations for Dolt databases.
//
// These functions are the "callable helper functions" for the Dog-driven
// mol-dog-reaper formula. They execute SQL operations but do not make
// eligibility decisions — the Dog (or daemon orchestrator) decides what
// to reap, purge, and auto-close based on the formula.
package reaper

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/steveyegge/gastown/internal/beads"
)

// validDBName matches safe database names (alphanumeric, underscore, hyphen).
var validDBName = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// DefaultDatabases is the static fallback list of known production databases.
// Used only when SHOW DATABASES fails (server unreachable).
// GH#2385: Removed legacy "gt" and "bd" names — modern towns use "hq" (town
// beads) and rig-specific names. Those databases no longer exist in most
// installations and their presence in the fallback caused phantom DB errors.
var DefaultDatabases = []string{"hq"}

// testPollutionPrefixes are database name prefixes created by tests.
var testPollutionPrefixes = []string{"testdb_", "beads_t", "beads_pt", "doctest_"}

// isNothingToCommit returns true if the error is a Dolt "nothing to commit" error.
func isNothingToCommit(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "nothing to commit")
}

// isTableNotFound returns true if the error indicates a missing table.
// This happens when beads stores its data on a separate Dolt instance from
// the gt Dolt server, so tables like issues/labels/dependencies don't exist
// on the server the reaper connects to.
func isTableNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "table not found") || strings.Contains(msg, "doesn't exist")
}

// isConnDeadErr reports whether err means the backend connection died — the
// gt-ybj signature. On gt-ybj/#11131 a DELETE against a row with a corrupt
// adaptive-TEXT value makes Dolt panic in its connection-handler goroutine
// (store/val/adaptive_value.go "invalid hash length: 19"); the panic is
// recovered server-side but the TCP connection is torn down, so the client sees
// "unexpected EOF" on the failing statement and "sql: connection is already
// closed" (sql.ErrConnDone) on any subsequent statement on that pinned conn.
//
// The best-effort purge uses this to detect a poison row: a delete whose
// connection dies marks its id-set as containing corruption to be bisected and
// quarantined, rather than failing the whole purge.
func isConnDeadErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, driver.ErrBadConn) || errors.Is(err, sql.ErrConnDone) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "invalid connection") ||
		strings.Contains(msg, "connection is already closed") ||
		strings.Contains(msg, "unexpected eof") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "connection reset")
}

// withConn runs fn against a single dedicated connection checked out from the
// pool, so that every session-stateful statement in a reaper write transaction
// (SET @@autocommit, the DELETE/UPDATEs, COMMIT, CALL DOLT_COMMIT) runs on the
// SAME backend connection.
//
// gt-ybj: issuing those statements against the pooled *sql.DB directly routed
// each one to an arbitrary connection. autocommit=0 leaked into pooled
// connections, the "transaction" was split across multiple backends, and a
// recycled connection surfaced on reuse as driver.ErrBadConn. Pinning one
// connection eliminates that. There is no retry here — a dead connection on the
// purge path means a Dolt panic on a corrupt row (#11131), which is
// deterministic; the best-effort purge handles it by bisecting and quarantining
// the poison row instead of blindly retrying.
func withConn(ctx context.Context, db *sql.DB, fn func(conn *sql.Conn) error) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection: %w", err)
	}
	defer func() {
		// Reset session state and return the connection to the pool. A dead
		// connection is discarded by Close(); a healthy one is recycled clean.
		_, _ = conn.ExecContext(context.Background(), "SET @@autocommit = 1")
		_ = conn.Close()
	}()
	return fn(conn)
}

func scanStaleCandidatesQuery(splitTarget bool) string {
	targetExpr := beads.DependencyTargetExpr("d", splitTarget)
	return fmt.Sprintf(`
		SELECT COUNT(*) FROM issues i
		WHERE i.status IN ('open', 'in_progress')
		AND i.updated_at < ?
		AND i.priority > 1
		AND i.issue_type != 'epic'
		AND i.id NOT IN (
			SELECT DISTINCT d.issue_id FROM dependencies d
			INNER JOIN issues dep ON %s = dep.id
			WHERE dep.status IN ('open', 'in_progress')
		)
		AND i.id NOT IN (
			SELECT DISTINCT %s FROM dependencies d
			INNER JOIN issues blocker ON d.issue_id = blocker.id
			WHERE blocker.status IN ('open', 'in_progress')
		)`, targetExpr, targetExpr)
}

func retryDependencyTargetQuery(query func(splitTarget bool) error) error {
	err := query(false)
	if err != nil && beads.IsDependencyTargetColumnError(err) {
		return query(true)
	}
	return err
}

func queryScanStaleCandidates(ctx context.Context, db *sql.DB, staleCutoff time.Time) (int, error) {
	var count int
	err := retryDependencyTargetQuery(func(splitTarget bool) error {
		return db.QueryRowContext(ctx, scanStaleCandidatesQuery(splitTarget), staleCutoff).Scan(&count)
	})
	return count, err
}

// DiscoverDatabases queries SHOW DATABASES on the Dolt server and returns
// all production databases, filtering out system databases and test pollution.
// Falls back to DefaultDatabases on any error.
func DiscoverDatabases(host string, port int) []string {
	dsn := fmt.Sprintf("root@tcp(%s:%d)/?parseTime=true&timeout=5s", host, port)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return DefaultDatabases
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rows, err := db.QueryContext(ctx, "SHOW DATABASES")
	if err != nil {
		return DefaultDatabases
	}
	defer rows.Close()

	var databases []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			continue
		}
		if name == "information_schema" || name == "mysql" {
			continue
		}
		lower := strings.ToLower(name)
		skip := false
		for _, prefix := range testPollutionPrefixes {
			if strings.HasPrefix(lower, prefix) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		databases = append(databases, name)
	}

	if len(databases) == 0 {
		return DefaultDatabases
	}
	return databases
}

// ScanResult holds the results of scanning a database for reaper candidates.
type ScanResult struct {
	Database        string    `json:"database"`
	ReapCandidates  int       `json:"reap_candidates"`
	PurgeCandidates int       `json:"purge_candidates"`
	MailCandidates  int       `json:"mail_candidates"`
	StaleCandidates int       `json:"stale_candidates"`
	OpenWisps       int       `json:"open_wisps"`
	Anomalies       []Anomaly `json:"anomalies,omitempty"`
}

// ReapResult holds the results of a reap operation.
type ReapResult struct {
	Database   string    `json:"database"`
	Reaped     int       `json:"reaped"`
	OpenRemain int       `json:"open_remain"`
	DryRun     bool      `json:"dry_run,omitempty"`
	Anomalies  []Anomaly `json:"anomalies,omitempty"`
}

// PurgeResult holds the results of a purge operation.
type PurgeResult struct {
	Database    string `json:"database"`
	WispsPurged int    `json:"wisps_purged"`
	MailPurged  int    `json:"mail_purged"`
	// WispsQuarantined / MailQuarantined are the total corrupt rows quarantined
	// for this database's wisps / issues tables — rows whose DELETE panics Dolt
	// on a corrupt adaptive-value encoding (gt-ybj/#11131). They are excluded
	// from the purge candidate set (and from "would purge" dry-run counts) and
	// skipped by every future purge, so they never re-panic. Always emitted (not
	// omitempty) so the count is visible even when zero.
	WispsQuarantined int `json:"wisps_quarantined"`
	MailQuarantined  int `json:"mail_quarantined"`
	// WispsNewlyQuarantined / MailNewlyQuarantined are the rows newly quarantined
	// during this run (a subset of the totals above).
	WispsNewlyQuarantined int       `json:"wisps_newly_quarantined,omitempty"`
	MailNewlyQuarantined  int       `json:"mail_newly_quarantined,omitempty"`
	DryRun                bool      `json:"dry_run,omitempty"`
	Anomalies             []Anomaly `json:"anomalies,omitempty"`
}

// ClosedEntry records an individual issue closure with details for logging.
type ClosedEntry struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	AgeDays  int    `json:"age_days"`
	Database string `json:"database"`
}

// AutoCloseResult holds the results of an auto-close operation.
type AutoCloseResult struct {
	Database      string        `json:"database"`
	Closed        int           `json:"closed"`
	ClosedEntries []ClosedEntry `json:"closed_entries,omitempty"`
	DryRun        bool          `json:"dry_run,omitempty"`
	Anomalies     []Anomaly     `json:"anomalies,omitempty"`
}

// Anomaly represents an unexpected condition found during reaper operations.
type Anomaly struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Count   int    `json:"count,omitempty"`
}

const (
	// DefaultQueryTimeout is the timeout for individual reaper SQL queries.
	DefaultQueryTimeout = 30 * time.Second
	// DefaultBatchSize is the number of rows per batch DELETE operation.
	DefaultBatchSize = 100
	// DefaultAlertThreshold is the open-wisp count above which callers should
	// surface a warning. Original sizing (800) assumed ~23 wisps/h × 24h TTL
	// ≈ 550 baseline, but observed HQ patrol velocity is ~40 wisps/h, putting
	// natural steady-state at ~960 — well above 800. Raised to 2000, then to
	// 3500 (gt-fdt) after dog-patrol step-wisps were found to hold a structural
	// baseline of ~2100 open wisps within their 24h TTL — above the old 2000
	// ceiling, so dogs escalated EVERY patrol cycle. 3500 gives headroom above
	// the structural baseline; the real fix is per-database or growth-rate-based
	// sizing per hq-o82zr.
	DefaultAlertThreshold = 3500
)

// ValidateDBName returns an error if the database name is unsafe.
func ValidateDBName(dbName string) error {
	if !validDBName.MatchString(dbName) {
		return fmt.Errorf("invalid database name: %q", dbName)
	}
	return nil
}

// OpenDB opens a connection to the Dolt server for a given database.
func OpenDB(host string, port int, dbName string, readTimeout, writeTimeout time.Duration) (*sql.DB, error) {
	if err := ValidateDBName(dbName); err != nil {
		return nil, err
	}
	dsn := fmt.Sprintf("root@tcp(%s:%d)/%s?parseTime=true&timeout=5s&readTimeout=%s&writeTimeout=%s",
		host, port, dbName,
		fmt.Sprintf("%ds", int(readTimeout.Seconds())),
		fmt.Sprintf("%ds", int(writeTimeout.Seconds())))
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	// Dolt's wait_timeout isn't reliably enforced — force the client to recycle
	// idle connections so daemon scan cycles don't accumulate hundreds of leaked
	// sleeping connections against the shared Dolt server.
	db.SetConnMaxIdleTime(30 * time.Second)
	db.SetConnMaxLifetime(2 * time.Minute)
	return db, nil
}

// parentExcludeJoin returns a LEFT JOIN clause and WHERE condition that restricts
// results to wisps whose parent molecule is closed, missing, or nonexistent.
//
// This replaces the previous parentCheckWhere() which used 3 correlated EXISTS
// subqueries per row, causing O(n*m) query cost on large wisp tables (gt-jd1z).
// The LEFT JOIN approach runs the subquery once and hash-joins: O(n+m).
//
// Semantics (unchanged from parentCheckWhere):
//   - No parent-child dependency → eligible (orphan wisps)
//   - Parent status is 'closed' → eligible (parent already reaped)
//   - Parent row missing (dangling ref) → eligible (parent already purged)
//
// The inverse is simpler: exclude wisps that have an OPEN parent.
//
// Usage:
//
//	join, where := parentExcludeJoin(splitTarget)
//	query := fmt.Sprintf("SELECT ... FROM wisps w %s WHERE ... AND %s", join, where)
func parentExcludeJoin(splitTarget bool) (joinClause, whereCondition string) {
	targetExpr := beads.DependencyTargetExpr("wd", splitTarget)
	joinClause = fmt.Sprintf(`LEFT JOIN (
		SELECT DISTINCT wd.issue_id
		FROM wisp_dependencies wd
		LEFT JOIN wisps pw ON pw.id = %s LEFT JOIN issues pi ON pi.id = %s
		WHERE wd.type = 'parent-child'
		AND (pw.status IN ('open', 'hooked', 'in_progress') OR pi.status IN ('open', 'in_progress'))
	) open_parent ON open_parent.issue_id = w.id`, targetExpr, targetExpr)
	whereCondition = "open_parent.issue_id IS NULL"
	return
}

func reapCandidatesQuery(splitTarget bool) string {
	parentJoin, parentWhere := parentExcludeJoin(splitTarget)
	return fmt.Sprintf(
		"SELECT COUNT(*) FROM wisps w %s WHERE w.status IN ('open', 'hooked', 'in_progress') AND w.created_at < ? AND w.issue_type != 'agent' AND %s",
		parentJoin, parentWhere)
}

func reapIDsQuery(splitTarget bool) string {
	parentJoin, parentWhere := parentExcludeJoin(splitTarget)
	whereClause := fmt.Sprintf(
		"w.status IN ('open', 'hooked', 'in_progress') AND w.created_at < ? AND w.issue_type != 'agent' AND %s", parentWhere)
	return fmt.Sprintf(
		"SELECT w.id FROM wisps w %s WHERE %s LIMIT %d",
		parentJoin, whereClause, DefaultBatchSize)
}

func danglingParentQuery(splitTarget bool) string {
	targetExpr := beads.DependencyTargetExpr("wd", splitTarget)
	return fmt.Sprintf(`
		SELECT COUNT(*) FROM wisp_dependencies wd
		LEFT JOIN wisps pw ON pw.id = %s LEFT JOIN issues pi ON pi.id = %s
		WHERE wd.type = 'parent-child' AND pw.id IS NULL AND pi.id IS NULL`, targetExpr, targetExpr)
}

func reverseWispDependencyDeleteQuery(inClause string, splitTarget bool) string {
	targetExpr := beads.DependencyTargetExpr("wisp_dependencies", splitTarget)
	return fmt.Sprintf("DELETE FROM wisp_dependencies WHERE %s IN %s", targetExpr, inClause)
}

// HasReaperSchema checks whether the database has the tables required for reaper
// operations (wisps and issues). Returns false (no error) when tables are missing
// — callers use this to skip databases that have incomplete beads schema (e.g.
// partially initialized databases on the central Dolt server).
func HasReaperSchema(db *sql.DB) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var count int
	err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM information_schema.tables WHERE table_name IN ('wisps', 'issues') AND table_schema = DATABASE()").Scan(&count)
	if err != nil {
		return false, fmt.Errorf("check reaper schema: %w", err)
	}
	return count >= 2, nil
}

// Scan counts reaper candidates in a database without modifying anything.
func Scan(db *sql.DB, dbName string, maxAge, purgeAge, mailDeleteAge, staleIssueAge time.Duration) (*ScanResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), DefaultQueryTimeout)
	defer cancel()

	result := &ScanResult{Database: dbName}
	now := time.Now().UTC()

	// Count reap candidates: open wisps past max_age with eligible parent status.
	// Must match Reap() eligibility semantics exactly, including the exclusion of
	// agent beads, otherwise scan can report candidates that reap will never close.
	// Uses LEFT JOIN anti-pattern instead of correlated EXISTS to avoid O(n*m) cost (gt-jd1z).
	if err := retryDependencyTargetQuery(func(splitTarget bool) error {
		return db.QueryRowContext(ctx, reapCandidatesQuery(splitTarget), now.Add(-maxAge)).Scan(&result.ReapCandidates)
	}); err != nil {
		return nil, fmt.Errorf("count reap candidates: %w", err)
	}

	// Count purge candidates: closed wisps past purge_age.
	// No parent check needed — closed wisps past the delete age are unconditionally purgeable.
	// The parent check (correlated subqueries on wisp_dependencies) was causing O(n*m) query
	// cost with 1800+ closed wisps, leading to CPU spikes and connection timeouts (gt-wvd2).
	purgeQuery := "SELECT COUNT(*) FROM wisps w WHERE w.status = 'closed' AND w.closed_at < ?"
	if err := db.QueryRowContext(ctx, purgeQuery, now.Add(-purgeAge)).Scan(&result.PurgeCandidates); err != nil {
		return nil, fmt.Errorf("count purge candidates: %w", err)
	}

	// Count mail candidates.
	// The issues/labels tables may not exist on the gt Dolt server if beads
	// stores its data on a separate Dolt instance. Skip gracefully.
	mailQuery := "SELECT COUNT(*) FROM issues WHERE status = 'closed' AND closed_at < ? AND id IN (SELECT issue_id FROM labels WHERE label = 'gt:message')"
	if err := db.QueryRowContext(ctx, mailQuery, now.Add(-mailDeleteAge)).Scan(&result.MailCandidates); err != nil {
		if !isTableNotFound(err) {
			return nil, fmt.Errorf("count mail candidates: %w", err)
		}
		// issues/labels table not on this server — skip mail count
	}

	// Count stale issue candidates.
	// Same caveat: issues/dependencies tables may live on a separate Dolt instance.
	staleCandidates, err := queryScanStaleCandidates(ctx, db, now.Add(-staleIssueAge))
	if err != nil {
		if !isTableNotFound(err) {
			return nil, fmt.Errorf("count stale candidates: %w", err)
		}
		// issues/dependencies table not on this server — skip stale count
	} else {
		result.StaleCandidates = staleCandidates
	}

	// Total open wisps.
	openQuery := "SELECT COUNT(*) FROM wisps WHERE status IN ('open', 'hooked', 'in_progress')"
	if err := db.QueryRowContext(ctx, openQuery).Scan(&result.OpenWisps); err != nil {
		return nil, fmt.Errorf("count open wisps: %w", err)
	}

	// Anomaly detection: dangling parent references.
	var danglingCount int
	if err := retryDependencyTargetQuery(func(splitTarget bool) error {
		return db.QueryRowContext(ctx, danglingParentQuery(splitTarget)).Scan(&danglingCount)
	}); err == nil && danglingCount > 0 {
		result.Anomalies = append(result.Anomalies, Anomaly{
			Type:    "dangling_parent_ref",
			Message: fmt.Sprintf("%d wisp(s) have parent dependency records pointing to purged/missing parents", danglingCount),
			Count:   danglingCount,
		})
	}

	return result, nil
}

// Reap closes stale wisps in a database whose parent molecule is already closed.
// UPDATEs are batched to avoid holding a write lock for extended periods on large tables.
func Reap(db *sql.DB, dbName string, maxAge time.Duration, dryRun bool) (*ReapResult, error) {
	// Use a longer timeout to accommodate batched processing across large tables.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	cutoff := time.Now().UTC().Add(-maxAge)

	result := &ReapResult{Database: dbName, DryRun: dryRun}

	if dryRun {
		if err := retryDependencyTargetQuery(func(splitTarget bool) error {
			return db.QueryRowContext(ctx, reapCandidatesQuery(splitTarget), cutoff).Scan(&result.Reaped)
		}); err != nil {
			return nil, fmt.Errorf("dry-run count: %w", err)
		}
		openQuery := "SELECT COUNT(*) FROM wisps WHERE status IN ('open', 'hooked', 'in_progress')"
		if err := db.QueryRowContext(ctx, openQuery).Scan(&result.OpenRemain); err != nil {
			return nil, fmt.Errorf("count open: %w", err)
		}
		return result, nil
	}

	// Batch UPDATE: select IDs in chunks, update each chunk.
	// This avoids holding a write lock on the entire table for minutes.
	// Uses LEFT JOIN anti-pattern instead of correlated EXISTS to avoid O(n*m) cost (gt-jd1z).
	// All statements run on one pinned connection so the autocommit=0 transaction
	// stays coherent on a single backend (gt-ybj).
	totalReaped := 0
	if err := withConn(ctx, db, func(conn *sql.Conn) error {
		totalReaped = 0

		if _, err := conn.ExecContext(ctx, "SET @@autocommit = 0"); err != nil {
			return fmt.Errorf("disable autocommit: %w", err)
		}

		for {
			var rows *sql.Rows
			err := retryDependencyTargetQuery(func(splitTarget bool) error {
				var queryErr error
				rows, queryErr = conn.QueryContext(ctx, reapIDsQuery(splitTarget), cutoff)
				return queryErr
			})
			if err != nil {
				return fmt.Errorf("select reap batch: %w", err)
			}

			var ids []string
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					rows.Close()
					return fmt.Errorf("scan wisp id: %w", err)
				}
				ids = append(ids, id)
			}
			rows.Close()

			if len(ids) == 0 {
				break
			}

			placeholders := make([]string, len(ids))
			args := make([]interface{}, len(ids))
			for i, id := range ids {
				placeholders[i] = "?"
				args[i] = id
			}
			inClause := strings.Join(placeholders, ",")

			updateQuery := fmt.Sprintf(
				"UPDATE wisps SET status='closed', closed_at=NOW() WHERE id IN (%s)",
				inClause)
			sqlResult, err := conn.ExecContext(ctx, updateQuery, args...)
			if err != nil {
				return fmt.Errorf("close stale wisps batch: %w", err)
			}

			affected, _ := sqlResult.RowsAffected()
			totalReaped += int(affected)
		}

		if totalReaped > 0 {
			// Flush the SQL transaction to the Dolt working set before DOLT_COMMIT.
			// With autocommit=0, UPDATE changes are in the SQL transaction buffer,
			// not the Dolt working set. DOLT_COMMIT operates on the working set,
			// so without this COMMIT it sees "nothing to commit".
			if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
				return fmt.Errorf("sql commit: %w", err)
			}
			commitMsg := fmt.Sprintf("reaper: close %d stale wisps in %s", totalReaped, dbName)
			if _, err := conn.ExecContext(ctx, fmt.Sprintf("CALL DOLT_COMMIT('-Am', '%s')", commitMsg)); err != nil { //nolint:gosec // G201: commitMsg from safe values
				// "nothing to commit" is expected when the reaper reverts dirty working
				// set changes back to match HEAD. The wisps were set to "open" in the
				// server's in-memory working set without being committed; closing them
				// makes the working set match HEAD again, so DOLT_COMMIT sees no diff.
				if !isNothingToCommit(err) {
					return fmt.Errorf("dolt commit: %w", err)
				}
			}
		}
		return nil
	}); err != nil {
		return result, err
	}

	result.Reaped = totalReaped

	openQuery := "SELECT COUNT(*) FROM wisps WHERE status IN ('open', 'hooked', 'in_progress')"
	if err := db.QueryRowContext(ctx, openQuery).Scan(&result.OpenRemain); err != nil {
		return result, fmt.Errorf("count open: %w", err)
	}

	return result, nil
}

// Purge deletes old closed wisps and mail from a database.
//
// Purge is best-effort (gt-ybj/#11131): rows whose DELETE panics Dolt because of
// a corrupt adaptive-value encoding are isolated by binary search, quarantined
// (so future purges skip them and never re-panic), and reported — the healthy
// rows are still deleted and the purge exits successfully. This keeps the
// reaper/hygiene molecule unblocked while the underlying data corruption is
// repaired separately.
func Purge(db *sql.DB, dbName string, purgeAge, mailDeleteAge time.Duration, dryRun bool) (*PurgeResult, error) {
	result := &PurgeResult{Database: dbName, DryRun: dryRun}

	// Purge closed wisps.
	purged, totalQ, newQ, anomalies, err := purgeClosedWisps(db, dbName, purgeAge, dryRun)
	if err != nil {
		return nil, fmt.Errorf("purge wisps: %w", err)
	}
	result.WispsPurged = purged
	result.WispsQuarantined = totalQ
	result.WispsNewlyQuarantined = newQ
	result.Anomalies = append(result.Anomalies, anomalies...)

	// Purge old mail.
	mailPurged, mailTotalQ, mailNewQ, err := purgeOldMail(db, dbName, mailDeleteAge, dryRun)
	if err != nil {
		return result, fmt.Errorf("purge mail: %w", err)
	}
	result.MailPurged = mailPurged
	result.MailQuarantined = mailTotalQ
	result.MailNewlyQuarantined = mailNewQ

	return result, nil
}

func purgeClosedWisps(db *sql.DB, dbName string, purgeAge time.Duration, dryRun bool) (deleted, totalQuarantined, newlyQuarantined int, anomalies []Anomaly, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	deleteCutoff := time.Now().UTC().Add(-purgeAge)

	// Load already-quarantined poison ids so they are excluded from BOTH the
	// digest (so dry-run "would purge" reflects only deletable rows) AND the
	// candidate set (so they are never re-attempted / re-panicked).
	preQuarantined, err := loadQuarantine(ctx, db, dbName, "wisps")
	if err != nil {
		return 0, 0, 0, nil, fmt.Errorf("load quarantine: %w", err)
	}

	// Digest: count purgeable closed wisps, EXCLUDING quarantined.
	// No parent check — closed wisps past the delete age are unconditionally purgeable.
	digestQuery := fmt.Sprintf(
		"SELECT COUNT(*) FROM wisps w WHERE w.status = 'closed' AND w.closed_at < ?%s",
		notInClause("w.id", len(preQuarantined)))
	args := append([]interface{}{deleteCutoff}, idArgs(preQuarantined)...)
	digestTotal := 0
	if scanErr := db.QueryRowContext(ctx, digestQuery, args...).Scan(&digestTotal); scanErr != nil {
		return 0, 0, 0, nil, fmt.Errorf("digest query: %w", scanErr)
	}

	if dryRun || digestTotal == 0 {
		return digestTotal, len(preQuarantined), 0, nil, nil
	}

	auxTables := []string{"wisp_labels", "wisp_comments", "wisp_events", "wisp_dependencies"}
	candidateQuery := func(quarantined []string) string {
		return fmt.Sprintf(
			"SELECT w.id FROM wisps w WHERE w.status = 'closed' AND w.closed_at < ?%s LIMIT %d",
			notInClause("w.id", len(quarantined)), DefaultBatchSize)
	}
	deleted, newlyQuarantined, anomalies, err = bestEffortPurge(ctx, db, dbName, "wisps", auxTables, true, candidateQuery, deleteCutoff, preQuarantined)
	return deleted, len(preQuarantined) + newlyQuarantined, newlyQuarantined, anomalies, err
}

func purgeOldMail(db *sql.DB, dbName string, mailDeleteAge time.Duration, dryRun bool) (deleted, totalQuarantined, newlyQuarantined int, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	mailCutoff := time.Now().UTC().Add(-mailDeleteAge)

	preQuarantined, err := loadQuarantine(ctx, db, dbName, "issues")
	if err != nil {
		if isTableNotFound(err) {
			return 0, 0, 0, nil // issues/labels not on this server
		}
		return 0, 0, 0, fmt.Errorf("load quarantine: %w", err)
	}

	countQuery := fmt.Sprintf(
		"SELECT COUNT(*) FROM `%s`.issues i WHERE i.status = 'closed' AND i.closed_at < ? AND i.id IN (SELECT issue_id FROM `%s`.labels WHERE label = 'gt:message')%s",
		dbName, dbName, notInClause("i.id", len(preQuarantined)))
	args := append([]interface{}{mailCutoff}, idArgs(preQuarantined)...)
	var count int
	if scanErr := db.QueryRowContext(ctx, countQuery, args...).Scan(&count); scanErr != nil {
		if isTableNotFound(scanErr) {
			return 0, 0, 0, nil // issues/labels not on this server
		}
		return 0, 0, 0, fmt.Errorf("count mail: %w", scanErr)
	}

	if dryRun || count == 0 {
		return count, len(preQuarantined), 0, nil
	}

	auxTables := []string{"labels", "comments", "events", "dependencies"}
	candidateQuery := func(quarantined []string) string {
		return fmt.Sprintf(
			"SELECT i.id FROM `%s`.issues i INNER JOIN `%s`.labels l ON i.id = l.issue_id WHERE i.status = 'closed' AND i.closed_at < ? AND l.label = 'gt:message'%s LIMIT %d",
			dbName, dbName, notInClause("i.id", len(quarantined)), DefaultBatchSize)
	}
	deleted, newlyQuarantined, _, err = bestEffortPurge(ctx, db, dbName, "issues", auxTables, false, candidateQuery, mailCutoff, preQuarantined)
	return deleted, len(preQuarantined) + newlyQuarantined, newlyQuarantined, err
}

// notInClause returns " AND <col> NOT IN (?,?,...)" with n placeholders, or "" if
// n == 0. Used to exclude already-quarantined poison ids from the candidate set
// so they are never re-attempted (and never re-panic).
func notInClause(col string, n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf(" AND %s NOT IN (%s)", col, strings.TrimSuffix(strings.Repeat("?,", n), ","))
}

// idArgs converts a slice of ids into a []interface{} for use as query bind args.
func idArgs(ids []string) []interface{} {
	args := make([]interface{}, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return args
}

func autoCloseWhereClause(dbName string, splitTarget bool) string {
	targetExpr := beads.DependencyTargetExpr("d", splitTarget)
	return fmt.Sprintf(`
		i.status IN ('open', 'in_progress')
		AND i.updated_at < ?
		AND i.priority > 1
		AND i.issue_type != 'epic'
		AND i.id NOT IN (
			SELECT DISTINCT l.issue_id FROM `+"`%s`"+`.labels l
			WHERE l.label IN ('gt:standing-orders', 'gt:keep', 'gt:role', 'gt:rig')
		)
		AND i.id NOT IN (
			SELECT DISTINCT d.issue_id FROM `+"`%s`"+`.dependencies d
			INNER JOIN `+"`%s`"+`.issues dep ON %s = dep.id
			WHERE dep.status IN ('open', 'in_progress')
		)
		AND i.id NOT IN (
			SELECT DISTINCT %s FROM `+"`%s`"+`.dependencies d
			INNER JOIN `+"`%s`"+`.issues blocker ON d.issue_id = blocker.id
			WHERE blocker.status IN ('open', 'in_progress')
		)`, dbName, dbName, dbName, targetExpr, targetExpr, dbName, dbName)
}

func autoCloseSelectQuery(dbName string, splitTarget bool) string {
	return fmt.Sprintf("SELECT i.id, i.title, i.updated_at FROM issues i WHERE %s", autoCloseWhereClause(dbName, splitTarget))
}

func queryAutoCloseCandidates(ctx context.Context, db *sql.DB, dbName string, staleCutoff time.Time) (*sql.Rows, error) {
	var rows *sql.Rows
	err := retryDependencyTargetQuery(func(splitTarget bool) error {
		var queryErr error
		rows, queryErr = db.QueryContext(ctx, autoCloseSelectQuery(dbName, splitTarget), staleCutoff)
		return queryErr
	})
	return rows, err
}

// AutoClose closes issues that have been open with no updates past staleAge.
// Excludes P0/P1 priority, epics, hooked/pinned issues, standing-order labels,
// and issues with active dependencies.
func AutoClose(db *sql.DB, dbName string, staleAge time.Duration, dryRun bool) (*AutoCloseResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), DefaultQueryTimeout)
	defer cancel()

	staleCutoff := time.Now().UTC().Add(-staleAge)
	result := &AutoCloseResult{Database: dbName, DryRun: dryRun}

	// Two-step SELECT-then-UPDATE to avoid self-referencing subquery in UPDATE,
	// which is not valid MySQL (Error 1093) and fragile in Dolt (dolthub/dolt#10600).
	rows, err := queryAutoCloseCandidates(ctx, db, dbName, staleCutoff)
	if err != nil {
		if isTableNotFound(err) {
			return result, nil // issues/dependencies not on this server
		}
		return nil, fmt.Errorf("select stale: %w", err)
	}
	type candidate struct {
		id        string
		title     string
		updatedAt time.Time
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.title, &c.updatedAt); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan stale id: %w", err)
		}
		candidates = append(candidates, c)
	}
	rows.Close()

	// Build per-issue closure log entries.
	now := time.Now().UTC()
	ids := make([]string, len(candidates))
	for i, c := range candidates {
		ids[i] = c.id
		result.ClosedEntries = append(result.ClosedEntries, ClosedEntry{
			ID:       c.id,
			Title:    c.title,
			AgeDays:  int(now.Sub(c.updatedAt).Hours() / 24),
			Database: dbName,
		})
	}

	if dryRun {
		result.Closed = len(ids)
		return result, nil
	}

	if len(ids) == 0 {
		return result, nil
	}

	placeholders := make([]string, len(ids))
	args := make([]interface{}, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	updateQuery := fmt.Sprintf(
		"UPDATE `%s`.issues SET status = 'closed', closed_at = NOW(), close_reason = 'stale:auto-closed by reaper' WHERE id IN (%s)",
		dbName, strings.Join(placeholders, ","))

	// One pinned connection for the whole autocommit=0 transaction (gt-ybj).
	if err := withConn(ctx, db, func(conn *sql.Conn) error {
		result.Anomalies = nil

		if _, err := conn.ExecContext(ctx, "SET @@autocommit = 0"); err != nil {
			return fmt.Errorf("disable autocommit: %w", err)
		}

		if _, err := conn.ExecContext(ctx, updateQuery, args...); err != nil {
			return fmt.Errorf("auto-close: %w", err)
		}

		// Flush SQL transaction to working set before DOLT_COMMIT.
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			result.Anomalies = append(result.Anomalies, Anomaly{
				Type:    "sql_commit_failed",
				Message: fmt.Sprintf("sql commit after auto-close failed: %v", err),
			})
			return nil
		}
		commitMsg := fmt.Sprintf("reaper: auto-close %d stale issues in %s", len(ids), dbName)
		if _, err := conn.ExecContext(ctx, fmt.Sprintf("CALL DOLT_COMMIT('-Am', '%s')", commitMsg)); err != nil { //nolint:gosec // G201: commitMsg from safe values
			// "nothing to commit" is expected when the updated tables are dolt_ignored.
			if !isNothingToCommit(err) {
				result.Anomalies = append(result.Anomalies, Anomaly{
					Type:    "dolt_commit_failed",
					Message: fmt.Sprintf("dolt commit after auto-close failed: %v", err),
				})
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}

	result.Closed = len(ids)

	return result, nil
}

// quarantineTable is the per-database bookkeeping table that records rows whose
// DELETE panics Dolt on a corrupt adaptive-value encoding (gt-ybj/#11131). Future
// purges exclude these ids so they are never re-attempted (and never re-panic).
// The table is created lazily, only when the first poison row is found, so
// uncorrupted databases never gain it.
const quarantineTable = "_gt_reaper_quarantine"

// bestEffortPurge deletes all candidate rows of primaryTable (+ auxTables),
// quarantining any "poison" row whose DELETE tears down the connection via a Dolt
// panic (gt-ybj/#11131). It returns the number deleted, the number newly
// quarantined this run, any anomalies, and an error only for non-corruption
// failures.
//
// candidateQuery(quarantined) must return a SELECT of up to DefaultBatchSize
// candidate ids, taking the delete cutoff as its first bind arg and excluding the
// supplied already-quarantined ids (via notInClause) so the batch always makes
// forward progress.
//
// reverseDep deletes dangling wisp_dependencies parent refs (wisps only).
//
// preQuarantined is the set of already-known poison ids (loaded by the caller);
// it is excluded from every candidate batch so those rows are never re-attempted.
func bestEffortPurge(ctx context.Context, db *sql.DB, dbName, primaryTable string, auxTables []string, reverseDep bool, candidateQuery func(quarantined []string) string, cutoff time.Time, preQuarantined []string) (deleted int, newlyQuarantinedCount int, anomalies []Anomaly, err error) {
	// Start from the already-quarantined set; grow it with poison found this run.
	quarantinedIDs := append([]string(nil), preQuarantined...)

	var newlyQuarantined []string
	for {
		// Select the next batch of candidate ids, excluding everything quarantined
		// (both pre-existing and found this run).
		args := make([]interface{}, 0, 1+len(quarantinedIDs))
		args = append(args, cutoff)
		for _, id := range quarantinedIDs {
			args = append(args, id)
		}
		idRows, qErr := db.QueryContext(ctx, candidateQuery(quarantinedIDs), args...)
		if qErr != nil {
			return deleted, len(newlyQuarantined), anomalies, fmt.Errorf("select batch: %w", qErr)
		}
		var ids []string
		for idRows.Next() {
			var id string
			if scanErr := idRows.Scan(&id); scanErr != nil {
				idRows.Close()
				return deleted, len(newlyQuarantined), anomalies, fmt.Errorf("scan id: %w", scanErr)
			}
			ids = append(ids, id)
		}
		idRows.Close()
		if len(ids) == 0 {
			break
		}

		// Delete the batch, isolating and quarantining poison rows by binary search.
		d, poison, bErr := bisectDelete(ids, func(chunk []string) error {
			return deleteIDSetTxn(ctx, db, chunk, primaryTable, auxTables, reverseDep)
		})
		deleted += d
		if bErr != nil {
			// A genuine (non-corruption) error — surface it but keep whatever we deleted.
			anomalies = append(anomalies, Anomaly{
				Type:    "delete_error",
				Message: fmt.Sprintf("delete %s batch: %v", primaryTable, bErr),
			})
			break
		}
		if len(poison) > 0 {
			newlyQuarantined = append(newlyQuarantined, poison...)
			quarantinedIDs = append(quarantinedIDs, poison...)
		}
	}

	// Persist newly-found poison rows so future purges skip them.
	if len(newlyQuarantined) > 0 {
		if qErr := quarantineIDs(ctx, db, dbName, primaryTable, newlyQuarantined); qErr != nil {
			anomalies = append(anomalies, Anomaly{
				Type:    "quarantine_failed",
				Message: fmt.Sprintf("failed to persist %d quarantined %s rows: %v", len(newlyQuarantined), primaryTable, qErr),
			})
		}
	}

	// Commit the deletions (and any quarantine inserts) to Dolt history.
	if deleted > 0 || len(newlyQuarantined) > 0 {
		if cErr := doltCommit(ctx, db, fmt.Sprintf("reaper: purge %s in %s (deleted %d, quarantined %d)", primaryTable, dbName, deleted, len(newlyQuarantined))); cErr != nil && !isNothingToCommit(cErr) {
			anomalies = append(anomalies, Anomaly{
				Type:    "dolt_commit_failed",
				Message: fmt.Sprintf("dolt commit after purge failed: %v", cErr),
			})
		}
	}

	return deleted, len(newlyQuarantined), anomalies, nil
}

// bisectDelete deletes the ids by calling tryDelete on the whole set, and on a
// connection-death (a Dolt panic on a corrupt row, gt-ybj/#11131) recursively
// bisects to isolate the poison id(s). Healthy chunks cost one tryDelete call;
// only chunks containing poison incur extra (log n) calls.
//
// Returns the count successfully deleted, the list of poison ids that must be
// quarantined, and a non-nil error only for a genuine non-corruption failure.
func bisectDelete(ids []string, tryDelete func(chunk []string) error) (deleted int, poison []string, err error) {
	if len(ids) == 0 {
		return 0, nil, nil
	}
	derr := tryDelete(ids)
	if derr == nil {
		return len(ids), nil, nil
	}
	if !isConnDeadErr(derr) {
		// Genuine error, not a poison-row panic — do not bisect.
		return 0, nil, derr
	}
	if len(ids) == 1 {
		// Single row whose DELETE panics Dolt → poison, quarantine it.
		return 0, ids, nil
	}
	mid := len(ids) / 2
	dL, pL, eL := bisectDelete(ids[:mid], tryDelete)
	if eL != nil {
		return dL, pL, eL
	}
	dR, pR, eR := bisectDelete(ids[mid:], tryDelete)
	return dL + dR, append(pL, pR...), eR
}

// deleteIDSetTxn deletes the given ids from primaryTable and auxTables in one
// pinned-connection autocommit=0 transaction, committing only on full success so
// a mid-transaction connection death rolls back cleanly. It returns an error for
// which isConnDeadErr is true when the backend connection dies (the gt-ybj/#11131
// poison-row panic), letting the caller bisect.
func deleteIDSetTxn(ctx context.Context, db *sql.DB, ids []string, primaryTable string, auxTables []string, reverseDep bool) error {
	if len(ids) == 0 {
		return nil
	}
	return withConn(ctx, db, func(conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, "SET @@autocommit = 0"); err != nil {
			return fmt.Errorf("disable autocommit: %w", err)
		}

		args := make([]interface{}, len(ids))
		placeholders := make([]string, len(ids))
		for i, id := range ids {
			args[i] = id
			placeholders[i] = "?"
		}
		inClause := "(" + strings.Join(placeholders, ",") + ")"

		// Auxiliary tables: tolerate non-connection errors (e.g. a missing aux
		// table on this server) but abort on a connection death so the caller
		// bisects rather than silently skipping the poison row.
		for _, tbl := range auxTables {
			delAux := fmt.Sprintf("DELETE FROM `%s` WHERE issue_id IN %s", tbl, inClause) //nolint:gosec // G201: tbl is internal
			if _, err := conn.ExecContext(ctx, delAux, args...); err != nil && isConnDeadErr(err) {
				return err
			}
		}

		if reverseDep {
			// Clean up reverse dependency references to prevent dangling parent refs.
			if err := retryDependencyTargetQuery(func(splitTarget bool) error {
				_, execErr := conn.ExecContext(ctx, reverseWispDependencyDeleteQuery(inClause, splitTarget), args...)
				return execErr
			}); err != nil && isConnDeadErr(err) {
				return err
			}
		}

		delPrimary := fmt.Sprintf("DELETE FROM `%s` WHERE id IN %s", primaryTable, inClause) //nolint:gosec // G201: primaryTable is internal
		if _, err := conn.ExecContext(ctx, delPrimary, args...); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return err
		}
		return nil
	})
}

// loadQuarantine returns the ids already quarantined for primaryTable in dbName.
// Returns an empty list (no error) when the quarantine table does not yet exist.
func loadQuarantine(ctx context.Context, db *sql.DB, dbName, primaryTable string) ([]string, error) {
	q := fmt.Sprintf("SELECT id FROM `%s`.`%s` WHERE table_name = ?", dbName, quarantineTable)
	rows, err := db.QueryContext(ctx, q, primaryTable)
	if err != nil {
		if isTableNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// quarantineIDs records poison ids in the per-database quarantine table, creating
// it lazily. The inserts land in the Dolt working set; the caller's doltCommit
// captures them along with the purge deletions.
func quarantineIDs(ctx context.Context, db *sql.DB, dbName, primaryTable string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	return withConn(ctx, db, func(conn *sql.Conn) error {
		createStmt := fmt.Sprintf("CREATE TABLE IF NOT EXISTS `%s`.`%s` ("+
			"table_name VARCHAR(64) NOT NULL, id VARCHAR(255) NOT NULL, db_name VARCHAR(128), "+
			"reason VARCHAR(255), quarantined_at DATETIME, PRIMARY KEY (table_name, id))", dbName, quarantineTable)
		if _, err := conn.ExecContext(ctx, createStmt); err != nil {
			return fmt.Errorf("create quarantine table: %w", err)
		}
		insertStmt := fmt.Sprintf("INSERT IGNORE INTO `%s`.`%s` (table_name, id, db_name, reason, quarantined_at) VALUES (?, ?, ?, ?, NOW())", dbName, quarantineTable)
		for _, id := range ids {
			if _, err := conn.ExecContext(ctx, insertStmt, primaryTable, id, dbName,
				"delete-panic: corrupt adaptive-value row (gt-ybj/#11131)"); err != nil {
				return fmt.Errorf("insert quarantine id %q: %w", id, err)
			}
		}
		return nil
	})
}

// doltCommit flushes the working set to Dolt history with the given message.
func doltCommit(ctx context.Context, db *sql.DB, message string) error {
	return withConn(ctx, db, func(conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, fmt.Sprintf("CALL DOLT_COMMIT('--allow-empty', '-Am', '%s')", message)) //nolint:gosec // G201: message from safe values
		return err
	})
}

// ClosePluginReceiptResult holds the results of closing plugin run receipts.
type ClosePluginReceiptResult struct {
	Database  string    `json:"database"`
	Closed    int       `json:"closed"`
	DryRun    bool      `json:"dry_run,omitempty"`
	Anomalies []Anomaly `json:"anomalies,omitempty"`
}

// ClosePluginReceipts closes open issues labeled "type:plugin-run" that are
// older than maxAge. These are transient run receipts created by deacon dog
// plugins; they should be closed shortly after creation since they exist only
// for audit/cooldown-gate purposes. The standard AutoClose path requires 7 days
// of staleness, which lets plugin receipts accumulate into the hundreds.
func ClosePluginReceipts(db *sql.DB, dbName string, maxAge time.Duration, dryRun bool) (*ClosePluginReceiptResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), DefaultQueryTimeout)
	defer cancel()

	cutoff := time.Now().UTC().Add(-maxAge)
	result := &ClosePluginReceiptResult{Database: dbName, DryRun: dryRun}

	// Find open issues with the "type:plugin-run" label older than maxAge.
	selectQuery := fmt.Sprintf(`
		SELECT i.id FROM `+"`%s`"+`.issues i
		INNER JOIN `+"`%s`"+`.labels l ON i.id = l.issue_id
		WHERE i.status IN ('open', 'in_progress')
		AND l.label = 'type:plugin-run'
		AND i.created_at < ?`, dbName, dbName)

	rows, err := db.QueryContext(ctx, selectQuery, cutoff)
	if err != nil {
		if isTableNotFound(err) {
			return result, nil
		}
		return nil, fmt.Errorf("select plugin receipts: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan plugin receipt id: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()

	result.Closed = len(ids)
	if len(ids) == 0 || dryRun {
		return result, nil
	}

	placeholders := make([]string, len(ids))
	args := make([]interface{}, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	updateQuery := fmt.Sprintf(
		"UPDATE `%s`.issues SET status = 'closed', closed_at = NOW() WHERE id IN (%s)",
		dbName, strings.Join(placeholders, ","))

	// One pinned connection for the whole autocommit=0 transaction (gt-ybj).
	if err := withConn(ctx, db, func(conn *sql.Conn) error {
		result.Anomalies = nil

		if _, err := conn.ExecContext(ctx, "SET @@autocommit = 0"); err != nil {
			return fmt.Errorf("disable autocommit: %w", err)
		}

		if _, err := conn.ExecContext(ctx, updateQuery, args...); err != nil {
			return fmt.Errorf("close plugin receipts: %w", err)
		}

		// Flush and commit.
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			result.Anomalies = append(result.Anomalies, Anomaly{
				Type:    "sql_commit_failed",
				Message: fmt.Sprintf("sql commit after plugin receipt close failed: %v", err),
			})
			return nil
		}
		commitMsg := fmt.Sprintf("reaper: close %d plugin receipts in %s", len(ids), dbName)
		if _, err := conn.ExecContext(ctx, fmt.Sprintf("CALL DOLT_COMMIT('-Am', '%s')", commitMsg)); err != nil { //nolint:gosec // G201: commitMsg from safe values
			if !isNothingToCommit(err) {
				result.Anomalies = append(result.Anomalies, Anomaly{
					Type:    "dolt_commit_failed",
					Message: fmt.Sprintf("dolt commit after plugin receipt close failed: %v", err),
				})
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}

	return result, nil
}

// ClosePluginDispatches closes open dispatch mail beads created by the daemon
// when sending plugin instructions to dogs. These beads are labeled "gt:message"
// + "from:daemon" with a title prefix "Plugin:" and are never closed after the
// dog completes. Without this, they accumulate at ~288/day (one per 5-minute
// stuck-agent-dog run) and are only caught by AutoClose after 7 days.
func ClosePluginDispatches(db *sql.DB, dbName string, maxAge time.Duration, dryRun bool) (*ClosePluginReceiptResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), DefaultQueryTimeout)
	defer cancel()

	cutoff := time.Now().UTC().Add(-maxAge)
	result := &ClosePluginReceiptResult{Database: dbName, DryRun: dryRun}

	// Find open issues with both "gt:message" and "from:daemon" labels whose
	// title starts with "Plugin:", older than maxAge.
	selectQuery := fmt.Sprintf(`
		SELECT i.id FROM `+"`%s`"+`.issues i
		INNER JOIN `+"`%s`"+`.labels l1 ON i.id = l1.issue_id
		INNER JOIN `+"`%s`"+`.labels l2 ON i.id = l2.issue_id
		WHERE i.status IN ('open', 'in_progress')
		AND l1.label = 'gt:message'
		AND l2.label = 'from:daemon'
		AND i.title LIKE 'Plugin:%%'
		AND i.created_at < ?`, dbName, dbName, dbName)

	rows, err := db.QueryContext(ctx, selectQuery, cutoff)
	if err != nil {
		if isTableNotFound(err) {
			return result, nil
		}
		return nil, fmt.Errorf("select plugin dispatches: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan plugin dispatch id: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()

	result.Closed = len(ids)
	if len(ids) == 0 || dryRun {
		return result, nil
	}

	placeholders := make([]string, len(ids))
	args := make([]interface{}, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	updateQuery := fmt.Sprintf(
		"UPDATE `%s`.issues SET status = 'closed', closed_at = NOW() WHERE id IN (%s)",
		dbName, strings.Join(placeholders, ","))

	// One pinned connection for the whole autocommit=0 transaction (gt-ybj).
	if err := withConn(ctx, db, func(conn *sql.Conn) error {
		result.Anomalies = nil

		if _, err := conn.ExecContext(ctx, "SET @@autocommit = 0"); err != nil {
			return fmt.Errorf("disable autocommit: %w", err)
		}

		if _, err := conn.ExecContext(ctx, updateQuery, args...); err != nil {
			return fmt.Errorf("close plugin dispatches: %w", err)
		}

		// Flush and commit.
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			result.Anomalies = append(result.Anomalies, Anomaly{
				Type:    "sql_commit_failed",
				Message: fmt.Sprintf("sql commit after plugin dispatch close failed: %v", err),
			})
			return nil
		}
		commitMsg := fmt.Sprintf("reaper: close %d plugin dispatches in %s", len(ids), dbName)
		if _, err := conn.ExecContext(ctx, fmt.Sprintf("CALL DOLT_COMMIT('-Am', '%s')", commitMsg)); err != nil { //nolint:gosec // G201: commitMsg from safe values
			if !isNothingToCommit(err) {
				result.Anomalies = append(result.Anomalies, Anomaly{
					Type:    "dolt_commit_failed",
					Message: fmt.Sprintf("dolt commit after plugin dispatch close failed: %v", err),
				})
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}

	return result, nil
}

// FormatJSON marshals any value to indented JSON.
func FormatJSON(v interface{}) string {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf(`{"error": %q}`, err.Error())
	}
	return string(data)
}
