// Package k8s is the MVP sei-sdk provider: it provisions SeiNetwork/SeiNode via
// controller-runtime server-side apply, stamps the canonical object labels,
// runs the canonical readiness probe (probe.go), and reads typed endpoints off
// .status (WS-E LLD §5). It registers itself as "k8s" via init(), so a consumer
// opts in with a blank import.
package k8s

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"

	"github.com/sei-protocol/sei-k8s-controller/sdk/sei"
	"github.com/sei-protocol/sei-k8s-controller/sdk/sei/provider"
)

func init() { provider.Register("k8s", New) }

// Provider is the k8s flavor. It is single-writer: the SSA field-owner is one
// manager, so a *sei.Client wrapping it is safe for sequential use only.
type Provider struct {
	c          ctrlclient.Client
	httpClient *http.Client
	defaultNS  string
}

// New is the registered Factory. It builds the controller-runtime client from
// the ambient kubeconfig chain (no caller dsn, §3.1) and a readiness-probe HTTP
// client that carries a client-side timeout — http.DefaultClient has none, and
// a hung node RPC would block a poll iteration past its budget (§5.5).
func New(ctx context.Context) (provider.Provider, error) {
	c, defaultNS, err := buildClient()
	if err != nil {
		return nil, &sei.Error{Class: sei.ClassInfra, Err: fmt.Errorf("building kube client: %w", err)}
	}
	return &Provider{
		c: c,
		// 30s is the deliberate MVP bound; per-call injection is deferred (§5.5).
		httpClient: &http.Client{Timeout: 30 * time.Second},
		defaultNS:  defaultNS,
	}, nil
}

// Name reports the registered flavor name.
func (p *Provider) Name() string { return "k8s" }

// Close releases provider resources. The k8s provider holds no long-lived
// connections, so this is a no-op.
func (p *Provider) Close() error { return nil }

// ProvisionNetwork SSA-applies the SeiNetwork and waits for PhaseReady.
func (p *Provider) ProvisionNetwork(ctx context.Context, spec sei.NetworkSpec) (sei.NetworkHandle, error) {
	ns := p.ns(spec.Namespace)
	net := renderNetwork(spec, ns)
	resource := fmt.Sprintf("SeiNetwork %s/%s", ns, net.Name)

	if err := p.apply(ctx, net, resource); err != nil {
		return nil, err
	}
	if err := p.waitNetworkReady(ctx, ns, net.Name, spec.ReadyTimeout); err != nil {
		return nil, err
	}
	// Re-read so the handle carries the Ready status (endpoints populated).
	fresh := &seiv1alpha1.SeiNetwork{}
	if err := p.c.Get(ctx, types.NamespacedName{Namespace: ns, Name: net.Name}, fresh); err != nil {
		return nil, &sei.Error{Class: sei.ClassInfra, Resource: resource,
			Err: fmt.Errorf("re-reading SeiNetwork post-Ready: %w", err)}
	}
	return &networkHandle{p: p, namespace: ns, name: net.Name, net: fresh}, nil
}

// ProvisionFleet SSA-applies N follower SeiNodes peered to net, waits for all to
// reach PhaseRunning, then runs the per-node readiness gate before returning.
// Serial fan-out: N is small, the apiserver is the bottleneck, and serial keeps
// the failure story precise (LLD §5.5). Un-defer a bounded errgroup at N>~20.
//
// On failure, best-effort deletes any SeiNodes it created so partial fleets
// don't orphan: SDK nodes carry no Workflow ownerRef, so nothing cascades for
// the caller (the FleetHandle is only returned on full success).
func (p *Provider) ProvisionFleet(ctx context.Context, net sei.NetworkHandle, spec sei.FleetSpec) (_ sei.FleetHandle, err error) {
	networkNS := net.Namespace()
	// FleetSpec.Namespace defaults to the network's namespace (spec.go doc), not
	// the provider default: peer discovery targets networkNS, so creating
	// followers elsewhere would leave discovery unable to find them.
	nodeNS := spec.Namespace
	if nodeNS == "" {
		nodeNS = networkNS
	}
	chainID, err := p.networkChainID(ctx, net)
	if err != nil {
		return nil, err
	}

	names := make([]string, 0, spec.Replicas)
	// Any error after the first apply cleans up the nodes created so far; cleanup
	// failure annotates but never masks the original provisioning error.
	defer func() {
		if err != nil {
			err = p.cleanupFleet(ctx, nodeNS, names, err)
		}
	}()

	for ordinal := 0; ordinal < spec.Replicas; ordinal++ {
		node := renderNode(spec, nodeNS, net.Name(), networkNS, chainID, ordinal)
		resource := fmt.Sprintf("SeiNode %s/%s", nodeNS, node.Name)
		if err = p.apply(ctx, node, resource); err != nil {
			return nil, err
		}
		names = append(names, node.Name)
	}

	if err = p.waitFleetRunning(ctx, nodeNS, names, spec.RunningTimeout, spec.PollInterval); err != nil {
		return nil, err
	}

	for _, name := range names {
		node := &seiv1alpha1.SeiNode{}
		resource := fmt.Sprintf("SeiNode %s/%s", nodeNS, name)
		if err = p.c.Get(ctx, types.NamespacedName{Namespace: nodeNS, Name: name}, node); err != nil {
			err = &sei.Error{Class: sei.ClassInfra, Resource: resource,
				Err: fmt.Errorf("re-reading SeiNode post-Running: %w", err)}
			return nil, err
		}
		ep := node.Status.Endpoint
		if ep == nil || ep.TendermintRpc == "" || ep.EvmJsonRpc == "" {
			err = &sei.Error{Class: sei.ClassInfra, Resource: resource, Phase: string(node.Status.Phase),
				Err: fmt.Errorf("running but .status.endpoint missing TM/EVM URLs")}
			return nil, err
		}
		if err = probeReady(ctx, p.httpClient, ep.TendermintRpc, ep.EvmJsonRpc, resource, spec.FirstBlockTimeout, spec.PollInterval); err != nil {
			return nil, err
		}
	}
	return &fleetHandle{p: p, namespace: nodeNS, names: names}, nil
}

// cleanupFleet best-effort deletes the SeiNodes ProvisionFleet created when
// provisioning fails partway. It returns the original provisioning error,
// annotated if a delete itself failed — orig stays primary so the caller still
// branches on the real cause (timeout/failed/infra), never on a cleanup hiccup.
func (p *Provider) cleanupFleet(ctx context.Context, ns string, names []string, orig error) error {
	var cleanupErrs []error
	for _, name := range names {
		obj := &seiv1alpha1.SeiNode{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
		if delErr := p.c.Delete(ctx, obj); delErr != nil && !apierrors.IsNotFound(delErr) {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("SeiNode %s/%s: %w", ns, name, delErr))
		}
	}
	if len(cleanupErrs) == 0 {
		return orig
	}
	return fmt.Errorf("%w (cleanup of partial fleet also failed: %w)", orig, errors.Join(cleanupErrs...))
}

// ns returns specNS or the provider default when specNS is empty.
func (p *Provider) ns(specNS string) string {
	if specNS != "" {
		return specNS
	}
	return p.defaultNS
}

// networkChainID resolves the chain ID followers join. The networkHandle caches
// the SeiNetwork object; a foreign NetworkHandle implementation falls back to a
// Get by name.
func (p *Provider) networkChainID(ctx context.Context, net sei.NetworkHandle) (string, error) {
	if nh, ok := net.(*networkHandle); ok && nh.net != nil {
		return nh.net.Spec.Genesis.ChainID, nil
	}
	obj := &seiv1alpha1.SeiNetwork{}
	if err := p.c.Get(ctx, types.NamespacedName{Namespace: net.Namespace(), Name: net.Name()}, obj); err != nil {
		return "", &sei.Error{Class: sei.ClassInfra, Resource: "SeiNetwork " + net.Name(),
			Err: fmt.Errorf("resolving chain ID: %w", err)}
	}
	return obj.Spec.Genesis.ChainID, nil
}

// apply server-side-applies obj under the SDK's field owner with ForceOwnership.
// AlreadyExists is not expected on SSA (apply is upsert); a conflict from a
// different manager surfaces as ClassInfra.
func (p *Provider) apply(ctx context.Context, obj ctrlclient.Object, resource string) error {
	err := p.c.Patch(ctx, obj, ctrlclient.Apply, fieldOwner, ctrlclient.ForceOwnership) //nolint:staticcheck // SA1019: SSA via ctrlclient.Apply intentionally matches the controller's established pattern (internal/task); module-wide migration to client.Client.Apply tracked separately
	if err == nil {
		return nil
	}
	if apierrors.IsConflict(err) {
		return &sei.Error{Class: sei.ClassInfra, Resource: resource,
			Err: fmt.Errorf("SSA conflict (another field manager owns a field): %w", err)}
	}
	return &sei.Error{Class: sei.ClassInfra, Resource: resource,
		Err: fmt.Errorf("server-side apply: %w", err)}
}

// waitNetworkReady polls until the SeiNetwork reaches GroupPhaseReady, failing
// fast on GroupPhaseFailed.
func (p *Provider) waitNetworkReady(ctx context.Context, ns, name string, timeout time.Duration) error {
	resource := fmt.Sprintf("SeiNetwork %s/%s", ns, name)
	return p.poll(ctx, timeout, defaultPoll, func(ctx context.Context) (bool, error) {
		net := &seiv1alpha1.SeiNetwork{}
		if err := p.c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, net); err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, &sei.Error{Class: sei.ClassInfra, Resource: resource, Err: err}
		}
		switch net.Status.Phase {
		case seiv1alpha1.GroupPhaseReady:
			return true, nil
		case seiv1alpha1.GroupPhaseFailed:
			return false, &sei.Error{Class: sei.ClassFailed, Resource: resource,
				Phase: string(net.Status.Phase), Err: fmt.Errorf("reached Failed phase")}
		default:
			return false, nil
		}
	}, resource, timeout)
}

// waitFleetRunning polls each named SeiNode until PhaseRunning, failing fast on
// PhaseFailed. All N share one deadline (mirrors provisionnode.waitForRunning).
func (p *Provider) waitFleetRunning(ctx context.Context, ns string, names []string, timeout, interval time.Duration) error {
	resource := fmt.Sprintf("SeiNode fleet %s/%v", ns, names)
	return p.poll(ctx, timeout, interval, func(ctx context.Context) (bool, error) {
		for _, name := range names {
			node := &seiv1alpha1.SeiNode{}
			nodeRes := fmt.Sprintf("SeiNode %s/%s", ns, name)
			if err := p.c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, node); err != nil {
				if apierrors.IsNotFound(err) {
					return false, nil
				}
				return false, &sei.Error{Class: sei.ClassInfra, Resource: nodeRes, Err: err}
			}
			switch node.Status.Phase {
			case seiv1alpha1.PhaseRunning:
				// done; check the rest
			case seiv1alpha1.PhaseFailed:
				return false, &sei.Error{Class: sei.ClassFailed, Resource: nodeRes,
					Phase: string(node.Status.Phase), Err: fmt.Errorf("reached Failed phase")}
			default:
				return false, nil
			}
		}
		return true, nil
	}, resource, timeout)
}

const defaultPoll = 2 * time.Second

// poll wraps wait.PollUntilContextTimeout, mapping a bare wait-timeout into a
// ClassTimeout SDK error while passing structured errors from cond through.
func (p *Provider) poll(ctx context.Context, timeout, interval time.Duration, cond wait.ConditionWithContextFunc, resource string, budget time.Duration) error {
	if interval == 0 {
		interval = defaultPoll
	}
	err := wait.PollUntilContextTimeout(ctx, interval, timeout, true, cond)
	if err == nil {
		return nil
	}
	// A structured SDK error from cond (fail-fast on Failed, infra Get error)
	// passes through unchanged; only a bare wait-timeout is relabeled.
	var sdkErr *sei.Error
	if errors.As(err, &sdkErr) {
		return err
	}
	return &sei.Error{Class: sei.ClassTimeout, Resource: resource,
		Err: fmt.Errorf("not ready within %s: %w", budget, err)}
}
