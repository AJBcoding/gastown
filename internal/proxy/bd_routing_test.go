package proxy

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExtractIDFlag(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no args", nil, ""},
		{"no --id", []string{"create", "--title", "x"}, ""},
		{"--id=value", []string{"create", "--id=in-foo", "--title", "x"}, "in-foo"},
		{"--id value", []string{"create", "--id", "in-foo", "--title", "x"}, "in-foo"},
		{"--id at end with no value", []string{"create", "--id"}, ""},
		{"--id=empty", []string{"create", "--id="}, ""},
		{"first --id wins", []string{"create", "--id=in-a", "--id=in-b"}, "in-a"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, extractIDFlag(tc.args))
		})
	}
}

// writeRoutes writes routes.jsonl into townRoot/.beads/.
// Each entry is one jsonl line.
func writeRoutes(t *testing.T, townRoot string, lines ...string) {
	t.Helper()
	beadsDir := filepath.Join(townRoot, ".beads")
	require.NoError(t, os.MkdirAll(beadsDir, 0755))
	content := ""
	for _, l := range lines {
		content += l + "\n"
	}
	require.NoError(t, os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(content), 0644))
}

func TestResolveBdCwd(t *testing.T) {
	t.Run("empty townRoot returns empty", func(t *testing.T) {
		assert.Equal(t, "", resolveBdCwd("", []string{"bd", "create", "--id=in-foo"}))
	})

	t.Run("short argv returns empty", func(t *testing.T) {
		assert.Equal(t, "", resolveBdCwd("/town", []string{"bd"}))
	})

	t.Run("non-bd command returns empty", func(t *testing.T) {
		assert.Equal(t, "", resolveBdCwd("/town", []string{"gt", "mail", "--id=in-foo"}))
	})

	t.Run("bd without --id returns empty", func(t *testing.T) {
		town := t.TempDir()
		writeRoutes(t, town, `{"prefix":"in-","path":"indigo"}`)
		assert.Equal(t, "", resolveBdCwd(town, []string{"bd", "create", "--title", "x"}))
	})

	t.Run("--id prefix not in routes returns empty", func(t *testing.T) {
		town := t.TempDir()
		writeRoutes(t, town, `{"prefix":"in-","path":"indigo"}`)
		assert.Equal(t, "", resolveBdCwd(town, []string{"bd", "create", "--id=zz-foo"}))
	})

	t.Run("town-level prefix (path=.) returns empty", func(t *testing.T) {
		town := t.TempDir()
		writeRoutes(t, town,
			`{"prefix":"hq-","path":"."}`,
			`{"prefix":"in-","path":"indigo"}`,
		)
		require.NoError(t, os.MkdirAll(filepath.Join(town, "indigo", ".beads"), 0755))
		assert.Equal(t, "", resolveBdCwd(town, []string{"bd", "create", "--id=hq-foo"}))
	})

	t.Run("rig-prefixed --id returns rig dir", func(t *testing.T) {
		town := t.TempDir()
		writeRoutes(t, town,
			`{"prefix":"hq-","path":"."}`,
			`{"prefix":"in-","path":"indigo"}`,
		)
		rigDir := filepath.Join(town, "indigo")
		require.NoError(t, os.MkdirAll(filepath.Join(rigDir, ".beads"), 0755))

		got := resolveBdCwd(town, []string{"bd", "create", "--id=in-foo", "--title", "x"})
		assert.Equal(t, rigDir, got)
	})

	t.Run("works with --id value (space-separated)", func(t *testing.T) {
		town := t.TempDir()
		writeRoutes(t, town, `{"prefix":"in-","path":"indigo"}`)
		rigDir := filepath.Join(town, "indigo")
		require.NoError(t, os.MkdirAll(filepath.Join(rigDir, ".beads"), 0755))

		got := resolveBdCwd(town, []string{"bd", "create", "--id", "in-foo"})
		assert.Equal(t, rigDir, got)
	})

	t.Run("bd at absolute path is recognized", func(t *testing.T) {
		town := t.TempDir()
		writeRoutes(t, town, `{"prefix":"in-","path":"indigo"}`)
		rigDir := filepath.Join(town, "indigo")
		require.NoError(t, os.MkdirAll(filepath.Join(rigDir, ".beads"), 0755))

		got := resolveBdCwd(town, []string{"/usr/local/bin/bd", "create", "--id=in-foo"})
		assert.Equal(t, rigDir, got)
	})
}
