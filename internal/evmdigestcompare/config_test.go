package evmdigestcompare

import (
	"os"
	"path/filepath"
	"testing"

	. "github.com/onsi/gomega"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfig(t *testing.T) {
	g := NewWithT(t)
	cfg, err := LoadConfig(writeConfig(t, `
groups:
- chain: arctic-1
  nodes:
  - {namespace: arctic-1, name: rpc-node-0, backend: composite}
  - {namespace: arctic-1, name: memiavl-rpc-0, backend: memiavl}
`))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(cfg.Groups).To(HaveLen(1))
	g.Expect(cfg.Groups[0].Nodes[0].Label()).To(Equal("arctic-1/rpc-node-0"))
	g.Expect(cfg.Groups[0].Nodes[0].BaseURL()).To(Equal("http://rpc-node-0-0.rpc-node-0.arctic-1.svc.cluster.local:8443"))
}

func TestConfigValidate(t *testing.T) {
	cases := map[string]string{
		"no groups": `groups: []`,
		"no chain": `
groups:
- nodes: [{namespace: a, name: x, backend: memiavl}, {namespace: a, name: y, backend: memiavl}]`,
		"one node": `
groups:
- chain: a
  nodes: [{namespace: a, name: x, backend: memiavl}]`,
		"bad backend": `
groups:
- chain: a
  nodes: [{namespace: a, name: x, backend: flatkv}, {namespace: a, name: y, backend: memiavl}]`,
		"missing name": `
groups:
- chain: a
  nodes: [{namespace: a, backend: memiavl}, {namespace: a, name: y, backend: memiavl}]`,
		"duplicate chain": `
groups:
- chain: a
  nodes: [{namespace: a, name: x, backend: memiavl}, {namespace: a, name: y, backend: memiavl}]
- chain: a
  nodes: [{namespace: a, name: z, backend: memiavl}, {namespace: a, name: w, backend: memiavl}]`,
		"node in two groups": `
groups:
- chain: a
  nodes: [{namespace: a, name: x, backend: memiavl}, {namespace: a, name: y, backend: memiavl}]
- chain: b
  nodes: [{namespace: a, name: x, backend: memiavl}, {namespace: a, name: w, backend: memiavl}]`,
		"unknown field": `
groups:
- chain: a
  pairs: []
  nodes: [{namespace: a, name: x, backend: memiavl}, {namespace: a, name: y, backend: memiavl}]`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := LoadConfig(writeConfig(t, body))
			NewWithT(t).Expect(err).To(HaveOccurred())
		})
	}
}
