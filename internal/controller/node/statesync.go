package node

import (
	"context"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// minCanonicalSyncers is the controller-side fail-closed floor: state-sync
// requires at least two configured canonical syncers before the
// state-sync-bearing plan may proceed (CometBFT needs >=2 rpc-servers, and we
// never fall back to peers as witnesses). Reliability comes from curating the
// canonical-syncer set, not from cross-witness checking — the sidecar keeps its
// existing trust-pinning behavior.
const minCanonicalSyncers = 2

// The controller only READS the canonical-syncer ConfigMap. The read-only
// intent is NOT yet enforced: the reconciler's pre-existing cluster-wide
// configmaps grant (controller.go) confers write access that controller-gen
// unions in, so this marker is documentary only. Scoping the controller to
// read-only on the trust root (so it cannot rewrite its own trust source) is a
// PLT-452 launch-gate blocker tracked in PLT-471, landing with the namespace
// lockdown that fixes the trust-root namespace.
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch

// reconcileStateSyncGate resolves the canonical-syncer set for a state-sync
// node and sets the always-present ConditionStateSyncReady accordingly. It
// mutates node.Status in-memory only — the condition and ResolvedStateSyncers
// are flushed by the caller's single optimistic-lock status patch, never a
// separate write. The caller runs it before the Failed/Paused early-returns so
// StateSyncReady is seeded on every path.
//
// Fail-closed is enforced downstream, not here: the planner declines to build a
// state-sync plan whenever StateSyncReady is not True (see ResolvePlan). That
// keeps ResolvePlan running on every reconcile, so handleTerminalPlan still
// clears terminal plans and non-state-sync work proceeds — this method only
// resolves the condition.
//
// A transient (non-NotFound) ConfigMap read error is NOT fatal: it sets
// StateSyncReady=Unknown/ConfigMapReadError and returns transient=true so the
// caller requeues without aborting the StatefulSet/Failed/Paused/flush path. A
// missing ConfigMap or <2 entries fails closed via False/NoSyncersConfigured.
func (r *SeiNodeReconciler) reconcileStateSyncGate(ctx context.Context, node *seiv1alpha1.SeiNode) (transient bool) {
	snap := node.Spec.SnapshotSource()
	if snap == nil || snap.StateSync == nil {
		// State-sync disabled: no state-sync task in the plan to gate.
		node.Status.ResolvedStateSyncers = nil
		setStateSyncReady(node, metav1.ConditionFalse, seiv1alpha1.ReasonStateSyncNotApplicable,
			"node does not enable state sync")
		return false
	}

	syncers, err := r.canonicalSyncers(ctx, node.Spec.ChainID)
	if err != nil {
		// Transient API error: fail closed (clear any stale set) but don't abort
		// the reconcile. Requeue and re-read next tick.
		node.Status.ResolvedStateSyncers = nil
		setStateSyncReady(node, metav1.ConditionUnknown, seiv1alpha1.ReasonStateSyncConfigMapReadError,
			fmt.Sprintf("reading canonical-syncer ConfigMap for chain %q: %v", node.Spec.ChainID, err))
		return true
	}

	if len(syncers) < minCanonicalSyncers {
		// Fail closed: do not feed a witness-less (or single-witness) set into
		// the plan. Leave ResolvedStateSyncers empty so a stale set can't leak
		// into ConfigureStateSyncTask on a later reconcile.
		node.Status.ResolvedStateSyncers = nil
		setStateSyncReady(node, metav1.ConditionFalse, seiv1alpha1.ReasonStateSyncNoSyncersConfigured,
			fmt.Sprintf("state sync requires >=%d canonical syncers configured for chain %q; found %d",
				minCanonicalSyncers, node.Spec.ChainID, len(syncers)))
		return false
	}

	if !slices.Equal(node.Status.ResolvedStateSyncers, syncers) {
		node.Status.ResolvedStateSyncers = syncers
	}
	setStateSyncReady(node, metav1.ConditionTrue, seiv1alpha1.ReasonStateSyncReady,
		fmt.Sprintf("%d canonical syncers configured for chain %q", len(syncers), node.Spec.ChainID))
	return false
}

// canonicalSyncers reads the canonical-syncer ConfigMap and returns the parsed
// syncer RPC endpoints for the given chain. A missing ConfigMap, an unset
// ConfigMap reference, or a chain with no entry all yield an empty slice (no
// error) so the caller fails closed via the StateSyncReady gate rather than
// crashing — state-sync is opt-in and the ConfigMap may legitimately be absent
// until GitOps provisions it.
//
// Data shape: data[chainID] is a list of `host:port` RPC endpoints separated by
// newlines and/or commas. Blank entries are dropped; the result is sorted and
// de-duplicated for a stable witness set.
func (r *SeiNodeReconciler) canonicalSyncers(ctx context.Context, chainID string) ([]string, error) {
	name := r.Platform.StateSyncSyncersConfigMap
	if strings.TrimSpace(name) == "" {
		return nil, nil
	}

	cm := &corev1.ConfigMap{}
	key := types.NamespacedName{Name: name, Namespace: r.Platform.StateSyncSyncersNamespace}
	if err := r.Get(ctx, key, cm); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}

	return parseSyncerList(cm.Data[chainID]), nil
}

// parseSyncerList splits a ConfigMap value on newlines and commas, trims
// whitespace, drops blanks, then sorts and de-duplicates.
func parseSyncerList(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == '\n' || r == '\r' || r == ',' || r == ' ' || r == '\t'
	})
	if len(fields) == 0 {
		return nil
	}
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f != "" {
			out = append(out, f)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// setStateSyncReady sets ConditionStateSyncReady with ObservedGeneration
// stamped, following the always-present condition discipline.
func setStateSyncReady(node *seiv1alpha1.SeiNode, status metav1.ConditionStatus, reason, message string) {
	apimeta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
		Type:               seiv1alpha1.ConditionStateSyncReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: node.Generation,
	})
}
