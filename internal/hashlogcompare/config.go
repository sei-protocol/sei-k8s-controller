package hashlogcompare

import (
	"errors"
	"fmt"
	"os"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/noderesource"
)

// Config lists the node/reserve pairs to compare.
type Config struct {
	Pairs []Pair `json:"pairs"`
}

// Pair is one migrating node compared against one reserve node of the same chain.
type Pair struct {
	Chain     string   `json:"chain"`
	Migrating Endpoint `json:"migrating"`
	Reserve   Endpoint `json:"reserve"`
}

// Endpoint names a SeiNode whose sidecar serves /v0/hashlog. URL, when set,
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

// BaseURL is the sidecar address the comparator calls.
func (e Endpoint) BaseURL() string {
	if e.URL != "" {
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
	if len(c.Pairs) == 0 {
		return errors.New("config has no pairs")
	}
	seen := make(map[string]struct{}, len(c.Pairs))
	for i, p := range c.Pairs {
		if p.Chain == "" {
			return fmt.Errorf("pairs[%d]: chain is required", i)
		}
		if err := p.Migrating.validate(); err != nil {
			return fmt.Errorf("pairs[%d].migrating: %w", i, err)
		}
		if err := p.Reserve.validate(); err != nil {
			return fmt.Errorf("pairs[%d].reserve: %w", i, err)
		}
		if p.Migrating.Label() == p.Reserve.Label() {
			return fmt.Errorf("pairs[%d]: migrating and reserve are the same node", i)
		}
		key := p.Chain + "|" + p.Migrating.Label() + "|" + p.Reserve.Label()
		if _, dup := seen[key]; dup {
			return fmt.Errorf("pairs[%d]: duplicate pair", i)
		}
		seen[key] = struct{}{}
	}
	return nil
}
