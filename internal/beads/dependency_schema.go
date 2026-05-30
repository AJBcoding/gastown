package beads

import (
	"fmt"
	"strings"
)

const (
	DependencyTargetAlias          = "depends_on_id"
	DependencyTargetIssueColumn    = "depends_on_issue_id"
	DependencyTargetWispColumn     = "depends_on_wisp_id"
	DependencyTargetExternalColumn = "depends_on_external"
)

var splitDependencyTargetColumns = []string{
	DependencyTargetIssueColumn,
	DependencyTargetWispColumn,
	DependencyTargetExternalColumn,
}

// DependencyTargetExpr returns the SQL expression for a dependency target.
// When splitTarget is true, bd stores targets in type-specific columns and the
// caller should treat the first non-null value as the legacy depends_on_id.
func DependencyTargetExpr(tableAlias string, splitTarget bool) string {
	if !splitTarget {
		return qualifyDependencyColumn(tableAlias, DependencyTargetAlias)
	}

	columns := make([]string, 0, len(splitDependencyTargetColumns))
	for _, column := range splitDependencyTargetColumns {
		columns = append(columns, qualifyDependencyColumn(tableAlias, column))
	}
	return "COALESCE(" + strings.Join(columns, ", ") + ")"
}

// DependencyTargetSelectExpr returns a SELECT expression whose result column is
// named depends_on_id for both legacy and split dependency-target schemas.
func DependencyTargetSelectExpr(tableAlias string, splitTarget bool) string {
	expr := DependencyTargetExpr(tableAlias, splitTarget)
	if !splitTarget {
		return expr
	}
	return expr + " AS " + DependencyTargetAlias
}

// DependencyTargetMatchExpr returns a WHERE predicate for a quoted SQL value.
// The quotedValue argument must already be a safe SQL string literal.
func DependencyTargetMatchExpr(tableAlias, quotedValue string, splitTarget bool) string {
	if !splitTarget {
		return DependencyTargetExpr(tableAlias, false) + " = " + quotedValue
	}

	columns := make([]string, 0, len(splitDependencyTargetColumns))
	for _, column := range splitDependencyTargetColumns {
		columns = append(columns, qualifyDependencyColumn(tableAlias, column))
	}
	return quotedValue + " IN (" + strings.Join(columns, ", ") + ")"
}

// DependencyTargetSplitColumns returns the typed target columns used by bd's
// split dependency-target schema, in INSERT order.
func DependencyTargetSplitColumns() string {
	return strings.Join(splitDependencyTargetColumns, ", ")
}

// DependencyTargetSplitValuesExprs returns SELECT expressions matching
// DependencyTargetSplitColumns. When the source dependency table is legacy, it
// classifies the legacy target using the same priority as bd migrations.
func DependencyTargetSplitValuesExprs(tableAlias string, splitSourceTarget bool) string {
	if splitSourceTarget {
		columns := make([]string, 0, len(splitDependencyTargetColumns))
		for _, column := range splitDependencyTargetColumns {
			columns = append(columns, qualifyDependencyColumn(tableAlias, column))
		}
		return strings.Join(columns, ", ")
	}

	target := qualifyDependencyColumn(tableAlias, DependencyTargetAlias)
	externalTarget := target + " LIKE 'external:%'"
	wispTargetExists := dependencyTargetIsWispExpr(target, "")
	issueTargetExists := fmt.Sprintf("EXISTS (SELECT 1 FROM issues target_issue WHERE target_issue.id = %s)", target)

	return strings.Join([]string{
		fmt.Sprintf("CASE WHEN NOT (%s) AND NOT (%s) AND %s THEN %s ELSE NULL END", externalTarget, wispTargetExists, issueTargetExists, target),
		fmt.Sprintf("CASE WHEN NOT (%s) AND %s THEN %s ELSE NULL END", externalTarget, wispTargetExists, target),
		fmt.Sprintf("CASE WHEN %s OR (NOT (%s) AND NOT (%s)) THEN %s ELSE NULL END", externalTarget, wispTargetExists, issueTargetExists, target),
	}, ", ")
}

// DependencyTargetSplitValuesExprsForWispCopy returns SELECT expressions for
// copying rows into wisp_dependencies with split target columns. migratingIDs is
// an optional, already-escaped SQL list without surrounding parentheses.
func DependencyTargetSplitValuesExprsForWispCopy(tableAlias string, splitSourceTarget bool, migratingIDs string) string {
	if !splitSourceTarget {
		target := qualifyDependencyColumn(tableAlias, DependencyTargetAlias)
		externalTarget := target + " LIKE 'external:%'"
		wispTargetExists := dependencyTargetIsWispExpr(target, migratingIDs)
		issueTargetExists := fmt.Sprintf("EXISTS (SELECT 1 FROM issues target_issue WHERE target_issue.id = %s)", target)

		return strings.Join([]string{
			fmt.Sprintf("CASE WHEN NOT (%s) AND NOT (%s) AND %s THEN %s ELSE NULL END", externalTarget, wispTargetExists, issueTargetExists, target),
			fmt.Sprintf("CASE WHEN NOT (%s) AND %s THEN %s ELSE NULL END", externalTarget, wispTargetExists, target),
			fmt.Sprintf("CASE WHEN %s OR (NOT (%s) AND NOT (%s)) THEN %s ELSE NULL END", externalTarget, wispTargetExists, issueTargetExists, target),
		}, ", ")
	}

	issueTarget := qualifyDependencyColumn(tableAlias, DependencyTargetIssueColumn)
	wispTarget := qualifyDependencyColumn(tableAlias, DependencyTargetWispColumn)
	externalTarget := qualifyDependencyColumn(tableAlias, DependencyTargetExternalColumn)
	issueMovesToWisp := fmt.Sprintf("%s IS NOT NULL AND (%s)", issueTarget, dependencyTargetIsWispExpr(issueTarget, migratingIDs))
	issueTargetExists := fmt.Sprintf("EXISTS (SELECT 1 FROM issues target_issue WHERE target_issue.id = %s)", issueTarget)

	return strings.Join([]string{
		fmt.Sprintf("CASE WHEN %s IS NOT NULL AND NOT (%s) AND %s THEN %s ELSE NULL END", issueTarget, issueMovesToWisp, issueTargetExists, issueTarget),
		fmt.Sprintf("CASE WHEN %s IS NOT NULL THEN %s WHEN %s THEN %s ELSE NULL END", wispTarget, wispTarget, issueMovesToWisp, issueTarget),
		fmt.Sprintf("CASE WHEN %s IS NOT NULL THEN %s WHEN %s IS NOT NULL AND NOT (%s) AND NOT (%s) THEN %s ELSE NULL END", externalTarget, externalTarget, issueTarget, issueMovesToWisp, issueTargetExists, issueTarget),
	}, ", ")
}

// IsDependencyTargetColumnError reports whether err came from a dependency
// target-column schema mismatch between legacy and split bd schemas.
func IsDependencyTargetColumnError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	missingColumn := strings.Contains(msg, "unknown column") ||
		strings.Contains(msg, "could not be found") ||
		strings.Contains(msg, "no such column")
	if !missingColumn {
		return false
	}
	if strings.Contains(msg, DependencyTargetAlias) {
		return true
	}
	for _, column := range splitDependencyTargetColumns {
		if strings.Contains(msg, column) {
			return true
		}
	}
	return false
}

// IsDependencyTargetGeneratedWriteError reports whether a write attempted to
// assign the generated legacy target column in bd's split dependency schema.
func IsDependencyTargetGeneratedWriteError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, DependencyTargetAlias) && strings.Contains(msg, "generated")
}

func qualifyDependencyColumn(tableAlias, column string) string {
	if tableAlias == "" {
		return column
	}
	return tableAlias + "." + column
}

func dependencyTargetIsWispExpr(target, migratingIDs string) string {
	exprs := []string{fmt.Sprintf("EXISTS (SELECT 1 FROM wisps target_wisp WHERE target_wisp.id = %s)", target)}
	if strings.TrimSpace(migratingIDs) != "" {
		exprs = append(exprs, fmt.Sprintf("%s IN (%s)", target, migratingIDs))
	}
	return strings.Join(exprs, " OR ")
}
