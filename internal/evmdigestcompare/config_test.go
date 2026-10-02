package evmdigestcompare

import (
	"testing"

	. "github.com/onsi/gomega"
)

const testChainName = "arctic-1"

func validConfig() Config {
	return Config{Chains: []Chain{{
		Name: testChainName,
		Nodes: []Node{
			{Endpoint: Endpoint{Namespace: testChainName, Name: "rpc-node-0-0"}, Backend: BackendComposite},
			{Endpoint: Endpoint{Namespace: testChainName, Name: "memiavl-rpc-0-0"}, Backend: BackendMemiavl},
		},
	}}}
}

func TestValidate(t *testing.T) {
	g := NewWithT(t)
	g.Expect(validConfig().Validate()).To(Succeed())
}

const testURL = "http://x"

func TestValidate_Rejects(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{"no chains", func(c *Config) { c.Chains = nil }},
		{"chain with one node", func(c *Config) { c.Chains[0].Nodes = c.Chains[0].Nodes[:1] }},
		{"chain with no name", func(c *Config) { c.Chains[0].Name = "" }},
		{"duplicate chain", func(c *Config) { c.Chains = append(c.Chains, c.Chains[0]) }},
		{"bad backend", func(c *Config) { c.Chains[0].Nodes[0].Backend = "rocksdb" }},
		{"missing node name", func(c *Config) { c.Chains[0].Nodes[0].Name = "" }},
		{"url and name both set", func(c *Config) { c.Chains[0].Nodes[0].URL = testURL }},
		{"duplicate node", func(c *Config) {
			c.Chains[0].Nodes = append(c.Chains[0].Nodes, c.Chains[0].Nodes[0])
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			cfg := validConfig()
			tt.mutate(&cfg)
			g.Expect(cfg.Validate()).To(HaveOccurred())
		})
	}
}

func TestEndpoint_Label(t *testing.T) {
	g := NewWithT(t)
	g.Expect((Endpoint{Namespace: "ns", Name: "n"}).Label()).To(Equal("ns/n"))
	g.Expect((Endpoint{URL: testURL}).Label()).To(Equal(testURL))
	g.Expect((Endpoint{Namespace: "ns", Name: "n"}).InCluster()).To(BeTrue())
	g.Expect((Endpoint{URL: testURL}).InCluster()).To(BeFalse())
}
