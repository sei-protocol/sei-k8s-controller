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

// TestNodepoolForMode pins the mode→pool routing: archive, validator and seed
// each get a dedicated pool; every other mode ("full", and the empty fallback)
// shares the default pool.
func TestNodepoolForMode(t *testing.T) {
	const (
		poolDefault   = "sei-node"
		poolArchive   = "sei-archive"
		poolValidator = "sei-validator"
		poolSeed      = "sei-seed"
	)
	c := Config{
		NodepoolName:      poolDefault,
		NodepoolArchive:   poolArchive,
		NodepoolValidator: poolValidator,
		NodepoolSeed:      poolSeed,
	}
	cases := []struct {
		mode string
		want string
	}{
		{"archive", poolArchive},
		{"validator", poolValidator},
		{"seed", poolSeed},
		{"full", poolDefault},
		{"", poolDefault},
	}
	for _, tc := range cases {
		if got := c.NodepoolForMode(tc.mode); got != tc.want {
			t.Errorf("NodepoolForMode(%q) = %q, want %q", tc.mode, got, tc.want)
		}
	}
}

// A seed must never inherit the RPC-class default pool: that costs an order of
// magnitude more than a seed is worth. NodepoolForMode reports the absence so
// the pod render can reject it.
func TestNodepoolForMode_SeedHasNoDefaultPoolFallback(t *testing.T) {
	c := Config{NodepoolName: "sei-node"}

	if got := c.NodepoolForMode(modeSeed); got != "" {
		t.Errorf("NodepoolForMode(seed) with no seed pool = %q, want empty", got)
	}
	if got := c.NodepoolForMode("full"); got != "sei-node" {
		t.Errorf("NodepoolForMode(full) = %q, want the default pool", got)
	}
}

// sizeSeed is optional: the code default is a correct value, so an app-config
// file predating the key still sizes a seed volume sanely.
func TestSeedStorageSize_FallsBackToCodeDefault(t *testing.T) {
	c := Config{}

	if got := c.SeedStorageSize(); got != defaultStorageSizeSeed {
		t.Errorf("SeedStorageSize() = %q, want %q", got, defaultStorageSizeSeed)
	}

	c.StorageSizeSeed = "40Gi"
	if got := c.SeedStorageSize(); got != "40Gi" {
		t.Errorf("SeedStorageSize() with override = %q, want 40Gi", got)
	}
}
