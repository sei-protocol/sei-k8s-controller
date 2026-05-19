package runner

import (
	"context"
	"fmt"
	"maps"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// SeiNodeGVR is the GroupVersionResource for SeiNode.
var SeiNodeGVR = schema.GroupVersionResource{
	Group:    "sei.io",
	Version:  "v1alpha1",
	Resource: "seinodes",
}

// DynamicNodeLister lists SeiNodes by label selector.
type DynamicNodeLister struct {
	Client dynamic.Interface
}

// List returns the names of all SeiNodes in namespace matching selector.
// Empty selector means list-all. Names are returned in apiserver order
// (typically by creation timestamp); the runner does not stably sort because
// the fanout policies are insensitive to ordering.
func (l DynamicNodeLister) List(ctx context.Context, namespace, selector string) ([]string, error) {
	opts := metav1.ListOptions{}
	if selector != "" {
		opts.LabelSelector = selector
	}
	list, err := l.Client.Resource(SeiNodeGVR).Namespace(namespace).List(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("list SeiNodes (selector=%q): %w", selector, err)
	}
	names := make([]string, 0, len(list.Items))
	for _, item := range list.Items {
		names = append(names, item.GetName())
	}
	return names, nil
}

// FanoutTarget is one rendered SeiNodeTask in a fan-out batch.
type FanoutTarget struct {
	// Node is the SeiNode name that .NODE was set to during render.
	Node string
	// Name is the deterministic metadata.name the runner applied.
	Name string
	// Manifest is the rendered SeiNodeTask manifest.
	Manifest []byte
}

// RenderFanout produces one FanoutTarget per node in nodes, varying only
// the .NODE template variable. baseVars is shared across all renders.
func RenderFanout(r Renderer, templatePath string, baseVars map[string]string, nodes []string) ([]FanoutTarget, error) {
	if len(nodes) == 0 {
		return nil, fmt.Errorf("fanout: selector matched zero SeiNodes")
	}
	out := make([]FanoutTarget, 0, len(nodes))
	for _, n := range nodes {
		vars := make(map[string]string, len(baseVars)+1)
		maps.Copy(vars, baseVars)
		vars["NODE"] = n
		manifest, name, err := r.Render(templatePath, vars)
		if err != nil {
			return nil, fmt.Errorf("render for node %q: %w", n, err)
		}
		out = append(out, FanoutTarget{Node: n, Name: name, Manifest: manifest})
	}
	return out, nil
}
