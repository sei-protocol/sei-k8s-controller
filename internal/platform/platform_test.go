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
