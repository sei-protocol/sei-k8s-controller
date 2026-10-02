package evmdigestcompare

import (
	"errors"
	"fmt"
	"os"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/noderesource"
	"github.com/sei-protocol/sei-k8s-controller/sidecarapi/wire"
)

// Config lists the groups of nodes whose EVM state digests must agree.
type Config struct {
	Groups []Group `json:"groups"`
}

// Group is a set of nodes of one chain compared against each other every round.
type Group struct {
	Chain string `json:"chain"`
	Nodes []Node `json:"nodes"`
}

// Node names a SeiNode in this cluster and the store layout its sidecar scans.
type Node struct {
	Namespace string                `json:"namespace"`
	Name      string                `json:"name"`
	Backend   wire.EVMDigestBackend `json:"backend"`
}

// Label identifies the node in metric labels and logs.
func (n Node) Label() string {
	return n.Namespace + "/" + n.Name
}

// BaseURL is the node's sidecar address behind its kube-rbac-proxy.
func (n Node) BaseURL() string {
	return noderesource.SidecarURLForNode(&seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Namespace: n.Namespace, Name: n.Name},
	})
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

// Validate rejects configs that would produce ambiguous metrics or let one
// node run two scans at once.
func (c Config) Validate() error {
	if len(c.Groups) == 0 {
		return errors.New("config has no groups")
	}
	chains := make(map[string]struct{}, len(c.Groups))
	nodes := make(map[string]struct{})
	for i, g := range c.Groups {
		if g.Chain == "" {
			return fmt.Errorf("groups[%d]: chain is required", i)
		}
		if _, dup := chains[g.Chain]; dup {
			return fmt.Errorf("groups[%d]: duplicate chain %q", i, g.Chain)
		}
		chains[g.Chain] = struct{}{}
		if len(g.Nodes) < 2 {
			return fmt.Errorf("groups[%d]: at least two nodes are required", i)
		}
		for j, n := range g.Nodes {
			if n.Namespace == "" || n.Name == "" {
				return fmt.Errorf("groups[%d].nodes[%d]: namespace and name are required", i, j)
			}
			if err := n.Backend.Validate(); err != nil {
				return fmt.Errorf("groups[%d].nodes[%d]: %w", i, j, err)
			}
			if _, dup := nodes[n.Label()]; dup {
				return fmt.Errorf("groups[%d].nodes[%d]: %s is listed more than once", i, j, n.Label())
			}
			nodes[n.Label()] = struct{}{}
		}
	}
	return nil
}
