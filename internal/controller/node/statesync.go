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
	ctrl "sigs.k8s.io/controller-runtime"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// minCanonicalSyncers is the controller-side fail-closed floor: state-sync
// requires at least two configured canonical syncers before the
// state-sync-bearing plan may proceed. This is a CONFIGURED-COUNT check; the
// >=2 byte-for-byte agreement check is the sidecar's job and lands with the
// seictl trust-hardening bump.
const minCanonicalSyncers = 2

// The controller only READS the canonical-syncer ConfigMap. The read-only
// intent is NOT yet enforced: the reconciler's pre-existing cluster-wide
// configmaps grant (controller.go) confers write access that controller-gen
// unions in, so this marker is documentary only. Scoping the controller to
// read-only on the trust root (so it cannot rewrite its own trust source) is a
// PLT-452 launch-gate blocker tracked in PLT-471, landing with the namespace
// lockdown that fixes the trust-root namespace.
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch

// enforceStateSyncGate is the fail-closed half of the state-sync gate. It runs
// AFTER reconcileStateSyncGate has resolved the always-present StateSyncReady
// condition and returned `ready`. When the node is not ready and has no active
// plan, it flushes the resolved condition via the caller's flushStatus closure
// (preserving the single-patch / optimistic-lock model), emits a Warning Event,
// and signals the reconciler to stop before ResolvePlan so a fail-closed node
// never builds the state-sync-bearing plan. stop=true means the plan loop must
// short-circuit; an already-active plan is left to run to completion (we don't
// yank a state-sync plan mid-execution on a transient ConfigMap blip — the gate
// re-asserts once the plan goes terminal). It makes no API calls itself, so it
// takes no context — resolution (the ConfigMap read) already happened.
func (r *SeiNodeReconciler) enforceStateSyncGate(
	node *seiv1alpha1.SeiNode,
	ready bool,
	flushStatus func() error,
) (_ ctrl.Result, stop bool, _ error) {
	planActive := node.Status.Plan != nil && node.Status.Plan.Phase == seiv1alpha1.TaskPlanActive
	if ready || planActive {
		return ctrl.Result{}, false, nil
	}
	if err := flushStatus(); err != nil {
		return ctrl.Result{}, true, fmt.Errorf("flushing state-sync gate status: %w", err)
	}
	r.Recorder.Eventf(node, corev1.EventTypeWarning, "StateSyncBlocked",
		"state sync enabled but <%d canonical syncers configured for chain %q; not building plan",
		minCanonicalSyncers, node.Spec.ChainID)
	return ctrl.Result{RequeueAfter: statusPollInterval}, true, nil
}

// reconcileStateSyncGate resolves the canonical-syncer set for a state-sync
// node and sets the always-present ConditionStateSyncReady accordingly. It
// mutates node.Status in-memory only — the condition and ResolvedStateSyncers
// are flushed by the caller's single optimistic-lock status patch, never a
// separate write. It is the resolution half of the gate: the caller runs it
// before the Failed/Paused early-returns so StateSyncReady is seeded on every
// path, then feeds `ready` into enforceStateSyncGate for fail-closed handling.
//
// It returns true ("ready to plan") when the state-sync-bearing plan may
// proceed: either state-sync is disabled (NotApplicable — the plan has no
// state-sync task to gate) or >=2 canonical syncers are configured (Ready).
// It returns false to fail closed: state-sync is enabled but <2 syncers are
// configured (NoSyncersConfigured), in which case the caller must skip
// ResolvePlan, emit a Warning Event, and requeue.
func (r *SeiNodeReconciler) reconcileStateSyncGate(ctx context.Context, node *seiv1alpha1.SeiNode) (bool, error) {
	snap := node.Spec.SnapshotSource()
	if snap == nil || snap.StateSync == nil {
		// State-sync disabled: no state-sync task in the plan to gate.
		node.Status.ResolvedStateSyncers = nil
		setStateSyncReady(node, metav1.ConditionFalse, seiv1alpha1.ReasonStateSyncNotApplicable,
			"node does not enable state sync")
		return true, nil
	}

	syncers, err := r.canonicalSyncers(ctx, node.Spec.ChainID)
	if err != nil {
		return false, fmt.Errorf("reading canonical-syncer ConfigMap: %w", err)
	}

	if len(syncers) < minCanonicalSyncers {
		// Fail closed: do not feed a witness-less (or single-witness) set into
		// the plan. Leave ResolvedStateSyncers empty so a stale set can't leak
		// into ConfigureStateSyncTask on a later reconcile.
		node.Status.ResolvedStateSyncers = nil
		setStateSyncReady(node, metav1.ConditionFalse, seiv1alpha1.ReasonStateSyncNoSyncersConfigured,
			fmt.Sprintf("state sync requires >=%d canonical syncers configured for chain %q; found %d",
				minCanonicalSyncers, node.Spec.ChainID, len(syncers)))
		return false, nil
	}

	if !slices.Equal(node.Status.ResolvedStateSyncers, syncers) {
		node.Status.ResolvedStateSyncers = syncers
	}
	setStateSyncReady(node, metav1.ConditionTrue, seiv1alpha1.ReasonStateSyncReady,
		fmt.Sprintf("%d canonical syncers configured for chain %q", len(syncers), node.Spec.ChainID))
	return true, nil
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
