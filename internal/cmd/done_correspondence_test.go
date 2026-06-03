package cmd

import (
	"strings"
	"testing"
)

// TestVerifyBranchBeadCorrespondence covers the gt-3vu guard that prevents a
// polecat on a stale branch from completing (and silently closing) the wrong bead.
func TestVerifyBranchBeadCorrespondence(t *testing.T) {
	tests := []struct {
		name      string
		branch    string
		issueID   string
		wantErr   bool
		wantInErr []string // substrings the error message must contain
	}{
		{
			name:    "matching branch and issue is fine",
			branch:  "polecat/cheedo/gt-3vu@mpy73kye",
			issueID: "gt-3vu",
			wantErr: false,
		},
		{
			name:    "matching without timestamp suffix is fine",
			branch:  "polecat/cheedo/gt-3vu",
			issueID: "gt-3vu",
			wantErr: false,
		},
		{
			name:      "stale branch from previous assignment is rejected",
			branch:    "polecat/onyx/cp-5ac@moqqb8ge",
			issueID:   "cp-2ip",
			wantErr:   true,
			wantInErr: []string{"cp-5ac", "cp-2ip", "stale"},
		},
		{
			name:    "modern timestamp-only branch has no encoded issue, allowed",
			branch:  "polecat/quartz-moqq4u0b",
			issueID: "cp-1u0",
			wantErr: false,
		},
		{
			name:    "empty issueID has nothing to compare, allowed",
			branch:  "polecat/cheedo/gt-3vu@mpy73kye",
			issueID: "",
			wantErr: false,
		},
		{
			name:    "subtask issue id matches",
			branch:  "polecat/cheedo/gt-3vu.1@abc",
			issueID: "gt-3vu.1",
			wantErr: false,
		},
		{
			name:      "subtask mismatch rejected",
			branch:    "polecat/cheedo/gt-3vu.1@abc",
			issueID:   "gt-3vu.2",
			wantErr:   true,
			wantInErr: []string{"gt-3vu.1", "gt-3vu.2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := verifyBranchBeadCorrespondence(tt.branch, tt.issueID)
			if tt.wantErr && err == nil {
				t.Fatalf("verifyBranchBeadCorrespondence(%q, %q) = nil, want error", tt.branch, tt.issueID)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("verifyBranchBeadCorrespondence(%q, %q) = %v, want nil", tt.branch, tt.issueID, err)
			}
			if err != nil {
				for _, want := range tt.wantInErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not contain %q", err.Error(), want)
					}
				}
			}
		})
	}
}
