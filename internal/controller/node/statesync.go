package node

import (
	"fmt"
	"slices"
	"strings"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/platform"
)

// minCanonicalSyncers is the controller-side fail-closed floor: state-sync
// requires at least two configured canonical syncers before the
// state-sync-bearing plan may proceed (CometBFT needs >=2 rpc-servers, and we
// never fall back to peers as witnesses). Reliability comes from curating the
// canonical-syncer set, not from cross-witness checking — the sidecar keeps its
// existing trust-pinning behavior.
const minCanonicalSyncers = 2

// reconcileStateSyncGate resolves the canonical-syncer set and sets the
// always-present ConditionStateSyncReady. It mutates node.Status in-memory only;
// the caller's single status patch flushes it. Run before the Failed/Paused
// early-returns so StateSyncReady is seeded on every path.
//
// Fail-closed is enforced downstream: the planner declines to build a state-sync
// plan whenever StateSyncReady is not True (see ResolvePlan), so this method only
// resolves the condition. It returns blocked=true whenever state-sync is enabled
// but the condition didn't resolve to True — both NoSyncersConfigured and the
// transient SyncerSourceError. The caller requeues on blocked: the syncer file is
// a mounted volume with no watch, so polling is the only way the gate re-resolves
// and unblocks once GitOps provisions or fixes the syncers.
func (r *SeiNodeReconciler) reconcileStateSyncGate(node *seiv1alpha1.SeiNode) (blocked bool) {
	snap := node.Spec.SnapshotSource()
	if snap == nil || snap.StateSync == nil {
		// State-sync disabled: no state-sync task in the plan to gate.
		node.Status.ResolvedStateSyncers = nil
		setStateSyncReady(node, metav1.ConditionFalse, seiv1alpha1.ReasonStateSyncNotApplicable,
			"node does not enable state sync")
		return false
	}

	syncers, err := r.canonicalSyncers(node.Spec.ChainID)
	if err != nil {
		// Transient read/parse error: fail closed (clear any stale set) but don't
		// abort the reconcile. Requeue and re-read next tick.
		node.Status.ResolvedStateSyncers = nil
		setStateSyncReady(node, metav1.ConditionUnknown, seiv1alpha1.ReasonStateSyncSyncerSourceError,
			fmt.Sprintf("reading canonical-syncer source for chain %q: %v", node.Spec.ChainID, err))
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
		return true
	}

	if !slices.Equal(node.Status.ResolvedStateSyncers, syncers) {
		node.Status.ResolvedStateSyncers = syncers
	}
	setStateSyncReady(node, metav1.ConditionTrue, seiv1alpha1.ReasonStateSyncReady,
		fmt.Sprintf("%d canonical syncers configured for chain %q", len(syncers), node.Spec.ChainID))
	return false
}

// canonicalSyncers reads the read-only application-config file fresh and
// returns the parsed syncer RPC endpoints for the given chain. An unset path, a
// missing file, or a chain with no entry all yield an empty slice (no error) so
// the caller fails closed via the StateSyncReady gate rather than crashing —
// state-sync is opt-in and the file may legitimately be absent until GitOps
// provisions the backing ConfigMap. Any other read or parse error is returned
// so the gate can treat it as transient.
//
// Endpoints are bare host:port (no scheme; the sidecar adds it); see
// platform.FileConfig for the file shape.
//
// Read fresh on every call: a mounted ConfigMap swaps atomically (a symlink flip
// on the directory mount), so re-reading picks up GitOps updates without a pod
// restart. Never cache an open handle.
func (r *SeiNodeReconciler) canonicalSyncers(chainID string) ([]string, error) {
	cfg, err := platform.ReadFileConfig(r.Platform.ControllerConfigFile)
	if err != nil {
		return nil, err
	}

	// A YAML list entry may itself carry comma/whitespace-joined endpoints, so
	// route the joined value through the same splitter the ConfigMap source used.
	return parseSyncerList(strings.Join(cfg.StateSync.Syncers[chainID], "\n")), nil
}

// parseSyncerList splits a syncer value on newlines and commas, trims
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
