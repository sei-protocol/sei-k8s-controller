package platform

import "testing"

// TestDataDirIsUnderHomeDir locks the home-convergence invariant: DataDir must
// be HomeDir/.sei so a bare `seid` (which resolves $HOME/.sei) lands on the
// data dir, with HomeDir the PARENT of the data dir and never the data dir
// itself (#449). noderesource.homeMountPath aliases HomeDir; this pins the
// relationship at the source constants.
func TestDataDirIsUnderHomeDir(t *testing.T) {
	if want := HomeDir + "/.sei"; DataDir != want {
		t.Fatalf("DataDir = %q, want %q (HomeDir/.sei)", DataDir, want)
	}
	if DataDir == HomeDir {
		t.Fatalf("DataDir must not equal HomeDir — HOME must be the data dir's parent (#449)")
	}
}

// TestNodepoolForMode pins the mode→pool routing: archive and validator each
// get a dedicated pool; every other mode ("full", and the empty fallback)
// shares the default pool.
func TestNodepoolForMode(t *testing.T) {
	const (
		poolDefault   = "sei-node"
		poolArchive   = "sei-archive"
		poolValidator = "sei-validator"
	)
	c := Config{
		NodepoolName:      poolDefault,
		NodepoolArchive:   poolArchive,
		NodepoolValidator: poolValidator,
	}
	cases := []struct {
		mode string
		want string
	}{
		{"archive", poolArchive},
		{"validator", poolValidator},
		{"full", poolDefault},
		{"", poolDefault},
	}
	for _, tc := range cases {
		if got := c.NodepoolForMode(tc.mode); got != tc.want {
			t.Errorf("NodepoolForMode(%q) = %q, want %q", tc.mode, got, tc.want)
		}
	}
}
