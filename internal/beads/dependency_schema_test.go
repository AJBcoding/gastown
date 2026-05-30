package beads

import (
	"errors"
	"strings"
	"testing"
)

func TestDependencyTargetExpr(t *testing.T) {
	tests := []struct {
		name        string
		tableAlias  string
		splitTarget bool
		want        string
	}{
		{
			name:        "legacy unqualified",
			splitTarget: false,
			want:        "depends_on_id",
		},
		{
			name:        "legacy qualified",
			tableAlias:  "d",
			splitTarget: false,
			want:        "d.depends_on_id",
		},
		{
			name:        "split qualified",
			tableAlias:  "d",
			splitTarget: true,
			want:        "COALESCE(d.depends_on_issue_id, d.depends_on_wisp_id, d.depends_on_external)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DependencyTargetExpr(tt.tableAlias, tt.splitTarget); got != tt.want {
				t.Fatalf("DependencyTargetExpr() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDependencyTargetSelectExprAliasesSplitTarget(t *testing.T) {
	got := DependencyTargetSelectExpr("", true)
	want := "COALESCE(depends_on_issue_id, depends_on_wisp_id, depends_on_external) AS depends_on_id"
	if got != want {
		t.Fatalf("DependencyTargetSelectExpr() = %q, want %q", got, want)
	}
}

func TestDependencyTargetMatchExpr(t *testing.T) {
	got := DependencyTargetMatchExpr("d", "'gt-target'", true)
	want := "'gt-target' IN (d.depends_on_issue_id, d.depends_on_wisp_id, d.depends_on_external)"
	if got != want {
		t.Fatalf("DependencyTargetMatchExpr() = %q, want %q", got, want)
	}
}

func TestDependencyTargetSplitValuesExprsCopiesTypedSourceColumns(t *testing.T) {
	got := DependencyTargetSplitValuesExprs("d", true)
	want := "d.depends_on_issue_id, d.depends_on_wisp_id, d.depends_on_external"
	if got != want {
		t.Fatalf("DependencyTargetSplitValuesExprs() = %q, want %q", got, want)
	}
}

func TestDependencyTargetSplitValuesExprsClassifiesLegacyTarget(t *testing.T) {
	got := DependencyTargetSplitValuesExprs("d", false)

	for _, want := range []string{
		"EXISTS (SELECT 1 FROM wisps target_wisp WHERE target_wisp.id = d.depends_on_id)",
		"EXISTS (SELECT 1 FROM issues target_issue WHERE target_issue.id = d.depends_on_id)",
		"d.depends_on_id LIKE 'external:%'",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("DependencyTargetSplitValuesExprs() missing %q in %q", want, got)
		}
	}
}

func TestDependencyTargetSplitValuesExprsForWispCopyReclassifiesMigratingSplitIssueTargets(t *testing.T) {
	got := DependencyTargetSplitValuesExprsForWispCopy("d", true, "'gt-moving'")

	for _, want := range []string{
		"d.depends_on_issue_id IN ('gt-moving')",
		"EXISTS (SELECT 1 FROM wisps target_wisp WHERE target_wisp.id = d.depends_on_issue_id)",
		"CASE WHEN d.depends_on_wisp_id IS NOT NULL THEN d.depends_on_wisp_id",
		"WHEN d.depends_on_issue_id IS NOT NULL AND (",
		"THEN d.depends_on_issue_id ELSE NULL END",
		"CASE WHEN d.depends_on_external IS NOT NULL THEN d.depends_on_external",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("DependencyTargetSplitValuesExprsForWispCopy() missing %q in %q", want, got)
		}
	}
}

func TestDependencyTargetSplitValuesExprsForWispCopyClassifiesLegacyMigratingTargets(t *testing.T) {
	got := DependencyTargetSplitValuesExprsForWispCopy("d", false, "'gt-moving'")

	if !strings.Contains(got, "d.depends_on_id IN ('gt-moving')") {
		t.Fatalf("legacy wisp-copy expression should treat migrating ids as wisp targets: %q", got)
	}
	if !strings.Contains(got, "EXISTS (SELECT 1 FROM wisps target_wisp WHERE target_wisp.id = d.depends_on_id)") {
		t.Fatalf("legacy wisp-copy expression should still classify existing wisps: %q", got)
	}
}

func TestIsDependencyTargetColumnError(t *testing.T) {
	err := errors.New(`query error: column "depends_on_id" could not be found in any table in scope`)
	if !IsDependencyTargetColumnError(err) {
		t.Fatal("expected depends_on_id missing-column error to be detected")
	}

	if IsDependencyTargetColumnError(errors.New("syntax error near dependencies")) {
		t.Fatal("unexpected dependency target column match for unrelated error")
	}
}

func TestIsDependencyTargetGeneratedWriteError(t *testing.T) {
	err := errors.New("Error 3105 (HY000): The value specified for generated column 'depends_on_id' in table 'wisp_dependencies' is not allowed.")
	if !IsDependencyTargetGeneratedWriteError(err) {
		t.Fatal("expected depends_on_id generated-column write error to be detected")
	}

	if IsDependencyTargetGeneratedWriteError(errors.New("Error 3105 (HY000): generated column 'other_column' cannot be assigned")) {
		t.Fatal("unexpected generated-column write match for unrelated column")
	}
	if IsDependencyTargetGeneratedWriteError(errors.New("syntax error near depends_on_id")) {
		t.Fatal("unexpected generated-column write match without generated marker")
	}
}
