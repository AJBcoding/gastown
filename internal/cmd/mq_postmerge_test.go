package cmd

import "testing"

// TestPlanMergeVerification covers the phantom-done decision logic (gt-hs8):
// gt mq post-merge must NOT close an MR unless it can confirm the merge commit
// landed on origin/<target>, or the caller explicitly opts out with --skip-verify.
func TestPlanMergeVerification(t *testing.T) {
	tests := []struct {
		name          string
		skipVerify    bool
		commitFlag    string
		mrTarget      string
		mrMergeCommit string
		defaultBranch string
		wantTarget    string
		wantSHA       string
		wantAction    mergeVerifyAction
	}{
		{
			name:          "commit flag verifies against MR target",
			commitFlag:    "abc123",
			mrTarget:      "main",
			defaultBranch: "trunk",
			wantTarget:    "main",
			wantSHA:       "abc123",
			wantAction:    mergeVerifyCheck,
		},
		{
			name:          "falls back to MR merge_commit when no flag",
			mrTarget:      "release",
			mrMergeCommit: "def456",
			defaultBranch: "main",
			wantTarget:    "release",
			wantSHA:       "def456",
			wantAction:    mergeVerifyCheck,
		},
		{
			name:          "commit flag takes precedence over MR merge_commit",
			commitFlag:    "flagsha",
			mrMergeCommit: "beadsha",
			mrTarget:      "main",
			defaultBranch: "main",
			wantTarget:    "main",
			wantSHA:       "flagsha",
			wantAction:    mergeVerifyCheck,
		},
		{
			name:          "falls back to default branch when MR has no target",
			commitFlag:    "abc123",
			mrTarget:      "",
			defaultBranch: "main",
			wantTarget:    "main",
			wantSHA:       "abc123",
			wantAction:    mergeVerifyCheck,
		},
		{
			name:          "no SHA available refuses closure",
			mrTarget:      "main",
			defaultBranch: "main",
			wantTarget:    "main",
			wantSHA:       "",
			wantAction:    mergeVerifyRefuseNoSHA,
		},
		{
			name:          "skip-verify bypasses even without a SHA",
			skipVerify:    true,
			mrTarget:      "main",
			defaultBranch: "main",
			wantTarget:    "main",
			wantSHA:       "",
			wantAction:    mergeVerifySkip,
		},
		{
			name:          "skip-verify wins over an available SHA",
			skipVerify:    true,
			commitFlag:    "abc123",
			mrTarget:      "main",
			defaultBranch: "main",
			wantTarget:    "main",
			wantSHA:       "",
			wantAction:    mergeVerifySkip,
		},
		{
			name:          "whitespace-only commit is treated as missing",
			commitFlag:    "   ",
			mrTarget:      "main",
			defaultBranch: "main",
			wantTarget:    "main",
			wantSHA:       "",
			wantAction:    mergeVerifyRefuseNoSHA,
		},
		{
			name:          "commit and target are trimmed",
			commitFlag:    "  abc123  ",
			mrTarget:      "  main  ",
			defaultBranch: "trunk",
			wantTarget:    "main",
			wantSHA:       "abc123",
			wantAction:    mergeVerifyCheck,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotTarget, gotSHA, gotAction := planMergeVerification(
				tt.skipVerify, tt.commitFlag, tt.mrTarget, tt.mrMergeCommit, tt.defaultBranch)
			if gotTarget != tt.wantTarget {
				t.Errorf("target = %q, want %q", gotTarget, tt.wantTarget)
			}
			if gotSHA != tt.wantSHA {
				t.Errorf("expectedSHA = %q, want %q", gotSHA, tt.wantSHA)
			}
			if gotAction != tt.wantAction {
				t.Errorf("action = %d, want %d", gotAction, tt.wantAction)
			}
		})
	}
}

func TestShortMQSHA(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"abc123", "abc123"},
		{"0123456789ab", "0123456789ab"},        // exactly 12, unchanged
		{"0123456789abcdef", "0123456789ab"},     // truncated to 12
		{"  0123456789abcdef  ", "0123456789ab"}, // trimmed then truncated
	}
	for _, tt := range tests {
		if got := shortMQSHA(tt.in); got != tt.want {
			t.Errorf("shortMQSHA(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
