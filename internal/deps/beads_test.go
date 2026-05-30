package deps

import "testing"

func TestParseBeadsVersion(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"bd version 0.55.4 (dev: main@3e1378e122c6)", "0.55.4"},
		{"bd version 0.55.4", "0.55.4"},
		{"bd version 1.2.3", "1.2.3"},
		{"bd version 10.20.30 (release)", "10.20.30"},
		{"some other output", ""},
		{"", ""},
	}

	for _, tt := range tests {
		result := parseBeadsVersion(tt.input)
		if result != tt.expected {
			t.Errorf("parseBeadsVersion(%q) = %q, want %q", tt.input, result, tt.expected)
		}
	}
}

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		a, b     string
		expected int
	}{
		{"0.55.4", "0.55.4", 0},
		{"0.55.4", "0.54.0", 1},
		{"0.54.0", "0.55.4", -1},
		{"1.0.0", "0.99.99", 1},
		{"0.55.5", "0.55.4", 1},
		{"0.55.4", "0.55.5", -1},
	}

	for _, tt := range tests {
		result := CompareVersions(tt.a, tt.b)
		if result != tt.expected {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", tt.a, tt.b, result, tt.expected)
		}
	}
}

func TestCheckBeadsVersionString(t *testing.T) {
	tests := []struct {
		name    string
		version string
		want    BeadsStatus
	}{
		{
			name:    "below minimum is too old",
			version: "1.0.3",
			want:    BeadsTooOld,
		},
		{
			name:    "minimum is accepted",
			version: MinBeadsVersion,
			want:    BeadsOK,
		},
		{
			name:    "maximum is accepted",
			version: MaxBeadsVersion,
			want:    BeadsOK,
		},
		{
			name:    "above maximum is too new",
			version: "1.0.5",
			want:    BeadsTooNew,
		},
		{
			name:    "future minor is too new",
			version: "1.1.0",
			want:    BeadsTooNew,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CheckBeadsVersionString(tt.version); got != tt.want {
				t.Fatalf("CheckBeadsVersionString(%q) = %v, want %v", tt.version, got, tt.want)
			}
		})
	}
}

func TestCheckBeads(t *testing.T) {
	// This test depends on whether bd is installed in the test environment
	status, version := CheckBeads()

	// We expect bd to be installed in dev environment
	if status == BeadsNotFound {
		t.Skip("bd not installed, skipping integration test")
	}

	if status == BeadsOK && version == "" {
		t.Error("CheckBeads returned BeadsOK but empty version")
	}

	t.Logf("CheckBeads: status=%d, version=%s", status, version)
}
