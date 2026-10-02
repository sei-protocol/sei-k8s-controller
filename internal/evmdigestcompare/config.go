package evmdigestcompare

import (
	"errors"
	"fmt"
	"os"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/noderesource"
)

// Digest backends a node scans with, matching `seidb evm-logical-digest
// --backend`: memiavl for reserve nodes, composite for migrating ones.
const (
	BackendMemiavl   = "memiavl"
	BackendComposite = "composite"
)

// Config lists the chains whose nodes are digested each round.
type Config struct {
	Chains []Chain `json:"chains"`
}

// Chain is one migration cell's digest set: migrating nodes plus memIAVL
// reserves, all digested at one shared height per round.
type Chain struct {
	Name  string `json:"chain"`
	Nodes []Node `json:"nodes"`
}

// Node is one digest participant: a sidecar endpoint and the backend its
// evm-digest task scans with.
type Node struct {
	Endpoint
	Backend string `json:"backend"`
}

// Endpoint names a SeiNode whose sidecar serves the task API. URL, when set,
// replaces the in-cluster address derived from Namespace and Name, for nodes
// that do not run as a SeiNode in this cluster.
type Endpoint struct {
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name,omitempty"`
	URL       string `json:"url,omitempty"`
}

// Label identifies the endpoint in metric labels and logs.
func (e Endpoint) Label() string {
	if e.Name != "" {
		return e.Namespace + "/" + e.Name
	}
	return e.URL
}

// InCluster reports whether the endpoint is a SeiNode sidecar in this cluster,
// reached through its kube-rbac-proxy.
func (e Endpoint) InCluster() bool {
	return e.URL == ""
}

// BaseURL is the sidecar address the comparator calls.
func (e Endpoint) BaseURL() string {
	if !e.InCluster() {
		return e.URL
	}
	return noderesource.SidecarURLForNode(&seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Namespace: e.Namespace, Name: e.Name},
	})
}

func (e Endpoint) validate() error {
	switch {
	case e.URL != "" && (e.Name != "" || e.Namespace != ""):
		return errors.New("set either url or namespace and name, not both")
	case e.URL != "":
		return nil
	case e.Name == "" || e.Namespace == "":
		return errors.New("namespace and name are required when url is unset")
	}
	return nil
}

// LoadConfig reads and validates a YAML or JSON config file.
func LoadConfig(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	if err := yaml.UnmarshalStrict(raw, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return cfg, cfg.Validate()
}

// Validate rejects configs that would produce ambiguous or empty metrics.
func (c Config) Validate() error {
	if len(c.Chains) == 0 {
		return errors.New("config has no chains")
	}
	seenChain := make(map[string]struct{}, len(c.Chains))
	for i, chain := range c.Chains {
		if chain.Name == "" {
			return fmt.Errorf("chains[%d]: chain is required", i)
		}
		if _, dup := seenChain[chain.Name]; dup {
			return fmt.Errorf("chains[%d]: duplicate chain %q", i, chain.Name)
		}
		seenChain[chain.Name] = struct{}{}
		if len(chain.Nodes) < 2 {
			return fmt.Errorf("chains[%d] (%s): at least two nodes are required to compare", i, chain.Name)
		}
		seenNode := make(map[string]struct{}, len(chain.Nodes))
		for j, n := range chain.Nodes {
			if err := n.validate(); err != nil {
				return fmt.Errorf("chains[%d].nodes[%d]: %w", i, j, err)
			}
			switch n.Backend {
			case BackendMemiavl, BackendComposite:
			default:
				return fmt.Errorf("chains[%d].nodes[%d] (%s): backend must be %s or %s",
					i, j, n.Label(), BackendMemiavl, BackendComposite)
			}
			if _, dup := seenNode[n.Label()]; dup {
				return fmt.Errorf("chains[%d].nodes[%d]: duplicate node %q", i, j, n.Label())
			}
			seenNode[n.Label()] = struct{}{}
		}
	}
	return nil
}
