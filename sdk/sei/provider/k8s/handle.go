package k8s

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"

	"github.com/sei-protocol/sei-k8s-controller/sdk/sei"
)

// networkHandle is the provider-side state behind a *sei.Network. It caches the
// last-read SeiNetwork so Endpoints() is a pure field read off the Ready status.
type networkHandle struct {
	p         *Provider
	namespace string
	name      string
	net       *seiv1alpha1.SeiNetwork
}

func (h *networkHandle) Name() string { return h.name }

// Namespace reports where the SeiNetwork lives, so ProvisionFleet wires
// followers' peer discovery at the genesis network's namespace rather than the
// provider default.
func (h *networkHandle) Namespace() string { return h.namespace }

// Endpoints projects SeiNetwork .status.endpoints into the EVM-only per-pod leaf
// plus aggregate TM scalars (LLD §5.4). Never reconstructs URLs.
func (h *networkHandle) Endpoints() sei.Endpoints {
	out := sei.Endpoints{}
	if h.net == nil || h.net.Status.Endpoints == nil {
		return out
	}
	ep := h.net.Status.Endpoints
	out.TendermintRPC = ep.TendermintRpc
	out.TendermintREST = ep.TendermintRest
	for _, n := range ep.Nodes {
		out.Nodes = append(out.Nodes, sei.NetworkNodeEndpoint{
			Name:       n.Name,
			EvmJsonRPC: n.EvmJsonRpc,
			EvmWS:      n.EvmWs,
		})
	}
	return out
}

// Teardown deletes the SeiNetwork. Idempotent: a NotFound is success.
func (h *networkHandle) Teardown(ctx context.Context) error {
	obj := &seiv1alpha1.SeiNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: h.name, Namespace: h.namespace},
	}
	if err := h.p.c.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
		return &sei.Error{Class: sei.ClassInfra, Resource: "SeiNetwork " + h.namespace + "/" + h.name,
			Err: fmt.Errorf("delete: %w", err)}
	}
	return nil
}

// fleetHandle is the provider-side state behind a *sei.Fleet. It holds the
// follower names; Endpoints() re-reads each node's .status.endpoint so the
// returned URLs reflect current status.
type fleetHandle struct {
	p         *Provider
	namespace string
	names     []string
}

// Endpoints reads each follower's 4-field .status.endpoint, stamping Name from
// the parent object (the status struct does not carry it). A node that has lost
// its endpoint contributes a name-only entry rather than being dropped, so the
// fleet ordering is stable. Read errors yield an empty bundle for that node —
// Endpoints is a status read, not a probe; ProvisionFleet already gated dial-
// readiness before returning.
func (h *fleetHandle) Endpoints() sei.FleetEndpoints {
	out := sei.FleetEndpoints{Nodes: make([]sei.FleetNodeEndpoint, 0, len(h.names))}
	for _, name := range h.names {
		entry := sei.FleetNodeEndpoint{Name: name}
		node := &seiv1alpha1.SeiNode{}
		if err := h.p.c.Get(context.Background(), types.NamespacedName{Namespace: h.namespace, Name: name}, node); err == nil {
			if ep := node.Status.Endpoint; ep != nil {
				entry.EvmJsonRPC = ep.EvmJsonRpc
				entry.EvmWS = ep.EvmWs
				entry.TendermintRPC = ep.TendermintRpc
				entry.TendermintREST = ep.TendermintRest
			}
		}
		out.Nodes = append(out.Nodes, entry)
	}
	return out
}

// Teardown deletes every follower SeiNode. Idempotent: NotFound is success, and
// it deletes whatever exists after a partial create.
func (h *fleetHandle) Teardown(ctx context.Context) error {
	for _, name := range h.names {
		obj := &seiv1alpha1.SeiNode{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: h.namespace},
		}
		if err := h.p.c.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			return &sei.Error{Class: sei.ClassInfra, Resource: "SeiNode " + h.namespace + "/" + name,
				Err: fmt.Errorf("delete: %w", err)}
		}
	}
	return nil
}
