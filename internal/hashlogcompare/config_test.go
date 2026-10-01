package hashlogcompare

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
pairs:
- chain: arctic-1
  migrating: {namespace: arctic-1, name: rpc-node-0}
  reserve: {namespace: arctic-1, name: memiavl-rpc-0}
- chain: atlantic-2
  migrating: {namespace: atlantic-2, name: node-wave-0}
  reserve: {url: "http://10.0.0.1:7777"}
`))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(cfg.Pairs).To(HaveLen(2))
	g.Expect(cfg.Pairs[0].Migrating.Label()).To(Equal("arctic-1/rpc-node-0"))
	g.Expect(cfg.Pairs[0].Migrating.BaseURL()).To(Equal("http://rpc-node-0-0.rpc-node-0.arctic-1.svc.cluster.local:8443"))
	g.Expect(cfg.Pairs[1].Reserve.Label()).To(Equal("http://10.0.0.1:7777"))
	g.Expect(cfg.Pairs[1].Reserve.BaseURL()).To(Equal("http://10.0.0.1:7777"))
	g.Expect(cfg.Pairs[0].Reserve.InCluster()).To(BeTrue())
	g.Expect(cfg.Pairs[1].Reserve.InCluster()).To(BeFalse())
}

func TestLoadConfig_Rejects(t *testing.T) {
	cases := map[string]string{
		"no pairs":      `pairs: []`,
		"unknown field": `{pairs: [{chain: c, migrating: {namespace: a, name: b}, reserve: {namespace: a, name: c}, x: 1}]}`,
		"no chain":      `{pairs: [{migrating: {namespace: a, name: b}, reserve: {namespace: a, name: c}}]}`,
		"no name":       `{pairs: [{chain: c, migrating: {namespace: a}, reserve: {namespace: a, name: c}}]}`,
		"url and name":  `{pairs: [{chain: c, migrating: {namespace: a, name: b}, reserve: {url: u, name: c}}]}`,
		"same node":     `{pairs: [{chain: c, migrating: {namespace: a, name: b}, reserve: {namespace: a, name: b}}]}`,
		"duplicate": `{pairs: [{chain: c, migrating: {namespace: a, name: b}, reserve: {namespace: a, name: c}},
			{chain: c, migrating: {namespace: a, name: b}, reserve: {namespace: a, name: c}}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := LoadConfig(writeConfig(t, body))
			NewWithT(t).Expect(err).To(HaveOccurred())
		})
	}
}
