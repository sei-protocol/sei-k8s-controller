package seinetwork

import (
	"context"
	"fmt"
	"maps"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// reconcileSeiNodes ensures the desired child SeiNodes exist with the desired
// spec (image/sidecar/overrides/configValues/labels propagated in-place every
// reconcile) and refreshes IncumbentNodes for the genesis planner. Mutations
// are skipped while the network is being deleted (so the cascade is not
// fought), while a plan is in progress (guarding the ceremony's child-Peers
// writes), or while paused.
func (r *SeiNetworkReconciler) reconcileSeiNodes(ctx context.Context, network *seiv1alpha1.SeiNetwork) error {
	// A deleting network mutates no child. Its children are being deleted with
	// it, so a create or an update here would recreate exactly what the cascade
	// is removing. Reconcile already routes a deleting network to
	// handleDeletion before it reaches this function; the guard lives here too
	// because this is the mutation site, and a later reordering upstream must
	// not be able to reintroduce the resurrect.
	if !network.DeletionTimestamp.IsZero() {
		return r.populateIncumbentNodes(ctx, network)
	}

	if network.Spec.Paused {
		return r.populateIncumbentNodes(ctx, network)
	}

	if !hasConditionTrue(network, seiv1alpha1.ConditionPlanInProgress) {
		for i := range int(network.Spec.Replicas) {
			if err := r.ensureSeiNode(ctx, network, i); err != nil {
				return fmt.Errorf("ensuring SeiNode %d: %w", i, err)
			}
		}
		if err := r.scaleDown(ctx, network); err != nil {
			return err
		}
	} else {
		log.FromContext(ctx).Info("plan in progress, skipping SeiNode mutations")
	}

	return r.populateIncumbentNodes(ctx, network)
}

// syncPausedToChildren brings every owned child SeiNode's Spec.Paused
// in line with desired. Children already in sync are left untouched.
func (r *SeiNetworkReconciler) syncPausedToChildren(ctx context.Context, network *seiv1alpha1.SeiNetwork, desired bool) error {
	children, err := r.listChildSeiNodes(ctx, network)
	if err != nil {
		return fmt.Errorf("listing child SeiNodes: %w", err)
	}
	for i := range children {
		child := &children[i]
		if child.Spec.Paused == desired {
			continue
		}
		child.Spec.Paused = desired
		if err := r.Update(ctx, child); err != nil {
			return fmt.Errorf("syncing paused=%t to %s: %w", desired, child.Name, err)
		}
		action := "ChildPaused"
		message := fmt.Sprintf("Paused SeiNode %s", child.Name)
		if !desired {
			action = "ChildResumed"
			message = fmt.Sprintf("Resumed SeiNode %s", child.Name)
		}
		r.Recorder.Event(network, corev1.EventTypeNormal, action, message)
	}
	return nil
}

// setGenesisCeremonyCondition stamps ConditionGenesisCeremonyComplete
// with the network's current genesis lifecycle state:
//
//   - True  / Complete       — ceremony finished (latched)
//   - False / InProgress     — ceremony executing under an active plan
//   - False / CeremonyFailed — last ceremony plan failed; resting between
//     failure and the auto-retry plan (set by failPlan, sticky until the
//     retry plan starts and PlanInProgress flips back to True)
//   - False / ValidatorLost  — ceremony abandoned because a founding validator
//     was deleted mid-plan; sticky like CeremonyFailed until the rebuilt plan
//     starts
//   - False / NotStarted     — ceremony not yet started
//
// Every SeiNetwork runs the ceremony (genesis is required), so there is no
// NotApplicable branch. Order matters: Complete latches; an active plan (the
// auto-retry) supersedes a prior CeremonyFailed so the condition tracks the
// live attempt rather than lying about a stale failure; CeremonyFailed is
// otherwise sticky so the failure survives the per-reconcile seed in the
// window before the retry plan is built (it must not silently reset to
// NotStarted, which is indistinguishable from "never ran").
func (r *SeiNetworkReconciler) setGenesisCeremonyCondition(network *seiv1alpha1.SeiNetwork) {
	if hasConditionTrue(network, seiv1alpha1.ConditionGenesisCeremonyComplete) {
		return
	}
	if hasConditionTrue(network, seiv1alpha1.ConditionPlanInProgress) {
		setCondition(network, seiv1alpha1.ConditionGenesisCeremonyComplete, metav1.ConditionFalse,
			"InProgress", "genesis ceremony is executing under an active plan")
		return
	}
	if hasConditionReason(network, seiv1alpha1.ConditionGenesisCeremonyComplete, "CeremonyFailed") ||
		hasConditionReason(network, seiv1alpha1.ConditionGenesisCeremonyComplete, ReasonValidatorLost) {
		return
	}
	setCondition(network, seiv1alpha1.ConditionGenesisCeremonyComplete, metav1.ConditionFalse,
		ReasonNotStarted, "genesis ceremony has not yet started")
}

// populateIncumbentNodes lists child SeiNodes and records their names on the
// network status. This is the genesis planner's child-list feed, refreshed
// each reconcile — NOT rollout state.
func (r *SeiNetworkReconciler) populateIncumbentNodes(ctx context.Context, network *seiv1alpha1.SeiNetwork) error {
	nodes, err := r.listChildSeiNodes(ctx, network)
	if err != nil {
		return fmt.Errorf("listing child SeiNodes: %w", err)
	}
	names := make([]string, 0, len(nodes))
	for i := range nodes {
		names = append(names, nodes[i].Name)
	}
	network.Status.IncumbentNodes = names
	return nil
}

func (r *SeiNetworkReconciler) ensureSeiNode(ctx context.Context, network *seiv1alpha1.SeiNetwork, ordinal int) error {
	desired := generateSeiNode(network, ordinal)
	if configValuesRejected(network) {
		desired.Spec.ConfigValues = nil
	}
	if err := ctrl.SetControllerReference(network, desired, r.Scheme); err != nil {
		return fmt.Errorf("setting owner reference: %w", err)
	}

	existing := &seiv1alpha1.SeiNode{}
	err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if apierrors.IsNotFound(err) {
		if createErr := r.Create(ctx, desired); createErr != nil {
			return createErr
		}
		r.Recorder.Eventf(network, corev1.EventTypeNormal, "SeiNodeCreated", "Created SeiNode %s", desired.Name)
		return nil
	}
	if err != nil {
		return err
	}

	// A child on its way out is left entirely alone. Under a Delete teardown the
	// network disappears before its children do — each holds the SeiNode
	// finalizer — so `kubectl delete --wait` returns, and the next run can
	// recreate this network while the previous generation's validators are still
	// terminating. Adopting one would rescue from the collector exactly the stale
	// validator the teardown just removed; re-speccing one would enroll a doomed
	// node in the new genesis ceremony. Once it is collected the Owns watch wakes
	// this reconcile and the Get above takes the create path.
	if !existing.DeletionTimestamp.IsZero() {
		log.FromContext(ctx).Info("child SeiNode is terminating; deferring until it is collected",
			"seinode", existing.Name)
		return nil
	}

	updated := false
	// The network's controller reference is reconciled on every pass, not only
	// at create, because a live child can be missing it: a Retain teardown
	// orphans children deliberately, and the next run recreates the same-named
	// SeiNetwork on top of them. Without re-adoption the controller manages a
	// child it does not own — it propagates image and labels below, while
	// garbage collection has no edge to walk, so a later Delete teardown leaves
	// the stale validator running and the next run collides with it.
	//
	// Only a child with no controller at all is adopted. ctrl.SetControllerReference
	// builds the reference but does not decide this: its already-owned check
	// matches an existing reference on name alone, so left to itself it would
	// rewrite a stale UID in place and take a child belonging to a previous
	// generation of this same-named network. A child that still names another
	// controller is a genuine conflict — two owners over one validator — and
	// fails loud rather than being fought over.
	switch controller := metav1.GetControllerOf(existing); {
	case controller == nil:
		if err := ctrl.SetControllerReference(network, existing, r.Scheme); err != nil {
			return fmt.Errorf("adopting SeiNode %s: %w", existing.Name, err)
		}
		r.Recorder.Eventf(network, corev1.EventTypeNormal, "SeiNodeAdopted",
			"Set owner reference on existing SeiNode %s", existing.Name)
		updated = true
	case controller.UID != network.UID:
		return fmt.Errorf("SeiNode %s is controlled by %s %s (uid %s), so this SeiNetwork (uid %s) will not adopt it",
			existing.Name, controller.Kind, controller.Name, controller.UID, network.UID)
	}
	if !maps.Equal(existing.Labels, desired.Labels) {
		existing.Labels = desired.Labels
		updated = true
	}
	if !maps.Equal(existing.Annotations, desired.Annotations) {
		existing.Annotations = desired.Annotations
		updated = true
	}
	if existing.Spec.Image != desired.Spec.Image {
		existing.Spec.Image = desired.Spec.Image
		updated = true
	}
	// Sync the whole Sidecar struct (image, port, AND resources) so any
	// spec.sidecar change propagates to children. Semantic.DeepEqual compares
	// resource.Quantity by value, not by its unexported internal repr.
	if !equality.Semantic.DeepEqual(existing.Spec.Sidecar, desired.Spec.Sidecar) {
		existing.Spec.Sidecar = desired.Spec.Sidecar
		updated = true
	}
	if !maps.Equal(existing.Spec.PodLabels, desired.Spec.PodLabels) {
		existing.Spec.PodLabels = desired.Spec.PodLabels
		updated = true
	}
	if !maps.Equal(existing.Spec.Overrides, desired.Spec.Overrides) {
		existing.Spec.Overrides = desired.Spec.Overrides
		updated = true
	}
	// The network's configValues are authoritative for the pool, so the whole
	// set is replaced rather than merged: a changed entry, a removed entry, a
	// cleared set, and a direct edit on the child all converge on the network's
	// set. Semantic.DeepEqual compares the apiextensions JSON values by bytes,
	// so a re-encode of an unchanged set is not a write.
	//
	// A set that cannot build an overlay is not stamped at all: it would fail
	// the same way on every validator, so the children keep their last good set
	// and go on taking image rolls while ConfigValuesValid carries the error.
	if !configValuesRejected(network) &&
		!equality.Semantic.DeepEqual(existing.Spec.ConfigValues, desired.Spec.ConfigValues) {
		existing.Spec.ConfigValues = desired.Spec.ConfigValues
		updated = true
	}
	// No identity / Peers / DataVolume / Resources sync below — deliberate, all
	// create-time only:
	//   - Peers are controller-owned: the genesis ceremony's collect-and-set-peers
	//     task patches each child's Spec.Peers with the assembled validator set
	//     (a StaticPeerSource). generateSeiNode emits empty peers at create, so
	//     syncing here would clobber the ceremony's writes every loop.
	//   - DataVolume backs a StatefulSet volumeClaimTemplate, which is immutable
	//     post-create; a post-create spec.dataVolume edit cannot take effect, so
	//     we do not attempt to sync it.
	//   - Resources is stamped once at child creation. The child's own
	//     spec.resources is CEL create-only, so writing a changed footprint here
	//     would be rejected by admission — and the Update below carries the whole
	//     spec, so a sync branch would fail the entire update, taking the image
	//     and podLabels sync down with it. The parent's spec.resources is
	//     create-only for the same reason, so there is nothing to sync anyway.
	if updated {
		return r.Update(ctx, existing)
	}
	return nil
}

// generateSeiNode constructs the desired child SeiNode for a given ordinal
// from the SeiNetwork's scalar genesis fields. Pure: depends only on the
// SeiNetwork spec and ordinal.
//
// It does NOT deep-copy a template. The validator is synthesized as a
// genesis-ceremony validator: SigningKey/NodeKey/OperatorKeyring/Snapshot are
// nil (the ceremony generates a distinct identity per replica), Peers is empty
// (the controller's collect-and-set-peers patches it in-place — see
// ensureSeiNode), and ExternalAddress / FullNode / Archive / Replayer are
// never set (external networking is GitOps-owned since PLT-451).
func generateSeiNode(network *seiv1alpha1.SeiNetwork, ordinal int) *seiv1alpha1.SeiNode {
	gc := network.Spec.Genesis

	podLabels := make(map[string]string, len(network.Spec.PodLabels)+2)
	maps.Copy(podLabels, network.Spec.PodLabels)
	podLabels[groupLabel] = network.Name
	podLabels[seinetworkLabel] = network.Name

	spec := seiv1alpha1.SeiNodeSpec{
		ChainID:      gc.ChainID,
		Image:        network.Spec.Image,
		Overrides:    maps.Clone(network.Spec.ConfigOverrides),
		ConfigValues: cloneConfigValues(network.Spec.ConfigValues),
		Sidecar:      network.Spec.Sidecar.DeepCopy(),
		DataVolume:   network.Spec.DataVolume.DeepCopy(),
		Resources:    network.Spec.Resources.DeepCopy(),
		PodLabels:    podLabels,
		Paused:       network.Spec.Paused,
		Validator: &seiv1alpha1.ValidatorSpec{
			GenesisCeremony: &seiv1alpha1.GenesisCeremonyNodeConfig{
				ChainID:        gc.ChainID,
				StakingAmount:  gc.StakingAmount,
				AccountBalance: gc.AccountBalance,
				Index:          int32(ordinal),
			},
		},
	}

	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:        seiNodeName(network, ordinal),
			Namespace:   network.Namespace,
			Labels:      seiNodeLabels(network, ordinal),
			Annotations: seiNodeAnnotations(network),
		},
		Spec: spec,
	}
}

// cloneConfigValues deep-copies the network's config values for a child spec,
// so a later write through the child cannot reach the network object. It
// returns nil for an empty set, keeping an unset field unset on the child.
func cloneConfigValues(values []seiv1alpha1.ConfigValue) []seiv1alpha1.ConfigValue {
	if len(values) == 0 {
		return nil
	}
	cloned := make([]seiv1alpha1.ConfigValue, len(values))
	for i := range values {
		values[i].DeepCopyInto(&cloned[i])
	}
	return cloned
}

// scaleDown deletes SeiNodes with ordinals >= the desired replica count.
func (r *SeiNetworkReconciler) scaleDown(ctx context.Context, network *seiv1alpha1.SeiNetwork) error {
	if network.Spec.Replicas <= 0 {
		log.FromContext(ctx).Info("refusing scale-down: desired replicas is zero or negative")
		return nil
	}

	nodeList := &seiv1alpha1.SeiNodeList{}
	if err := r.List(ctx, nodeList,
		client.InNamespace(network.Namespace),
		client.MatchingLabels(seinetworkSelector(network)),
	); err != nil {
		return fmt.Errorf("listing child SeiNodes: %w", err)
	}

	for i := range nodeList.Items {
		node := &nodeList.Items[i]
		if !metav1.IsControlledBy(node, network) {
			continue
		}
		if node.Labels[seinetworkOrdinalLabel] == "" {
			continue
		}
		var ord int
		if _, err := fmt.Sscanf(node.Labels[seinetworkOrdinalLabel], "%d", &ord); err != nil {
			continue
		}
		if ord >= int(network.Spec.Replicas) {
			if err := r.Delete(ctx, node); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("deleting excess SeiNode %s: %w", node.Name, err)
			}
			r.Recorder.Eventf(network, corev1.EventTypeNormal, "SeiNodeDeleted", "Scaled down SeiNode %s", node.Name)
		}
	}
	return nil
}

func (r *SeiNetworkReconciler) listChildSeiNodes(ctx context.Context, network *seiv1alpha1.SeiNetwork) ([]seiv1alpha1.SeiNode, error) {
	nodeList := &seiv1alpha1.SeiNodeList{}
	if err := r.List(ctx, nodeList,
		client.InNamespace(network.Namespace),
		client.MatchingLabels(seinetworkSelector(network)),
	); err != nil {
		return nil, fmt.Errorf("listing child SeiNodes: %w", err)
	}
	owned := nodeList.Items[:0]
	for i := range nodeList.Items {
		if metav1.IsControlledBy(&nodeList.Items[i], network) {
			owned = append(owned, nodeList.Items[i])
		}
	}
	return owned, nil
}

// retainReason is the operator-facing reason stamped on every child a Retain
// teardown releases. It is written to the child, not the network, because the
// network is gone moments later and the child is what an operator finds.
const retainReason = "SeiNetwork deleted with deletionPolicy=Retain; the data volume holds the " +
	"ceremony-generated consensus identity, which cannot be regenerated"

// orphanChildSeiNodes strips the network owner ref so children survive
// SeiNetwork deletion under DeletionPolicyRetain, and records the decision on
// each child: the retain annotations go in the same patch as the owner-ref
// removal so a child is never released without its reason, and an Event on the
// child names the network that released it.
func (r *SeiNetworkReconciler) orphanChildSeiNodes(ctx context.Context, network *seiv1alpha1.SeiNetwork) error {
	nodes, err := r.listChildSeiNodes(ctx, network)
	if err != nil {
		return err
	}
	for i := range nodes {
		node := &nodes[i]
		if err := r.retain(ctx, node, network); err != nil {
			return fmt.Errorf("orphaning SeiNode %s: %w", node.Name, err)
		}
		r.Recorder.Eventf(node, corev1.EventTypeNormal, "RetainedByDeletionPolicy",
			"Released from SeiNetwork %s: %s", network.Name, retainReason)
	}
	return nil
}

// retain releases obj from network in one patch: the network's owner
// reference goes and the retain annotations arrive together, so an ownerless
// object is never found without its reason. Re-running it is a no-op patch.
func (r *SeiNetworkReconciler) retain(ctx context.Context, obj client.Object, network *seiv1alpha1.SeiNetwork) error {
	patch := client.MergeFrom(obj.DeepCopyObject().(client.Object))
	refs := obj.GetOwnerReferences()
	filtered := make([]metav1.OwnerReference, 0, len(refs))
	for _, ref := range refs {
		if ref.UID != network.UID {
			filtered = append(filtered, ref)
		}
	}
	obj.SetOwnerReferences(filtered)
	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string, 2)
	}
	annotations[seiv1alpha1.RetainedFromAnnotation] = network.Name
	annotations[seiv1alpha1.RetainReasonAnnotation] = retainReason
	obj.SetAnnotations(annotations)
	return r.Patch(ctx, obj, patch)
}

// clearRetainRecord removes the retain annotations from an object a network
// owns again. No-op when neither is present.
func (r *SeiNetworkReconciler) clearRetainRecord(ctx context.Context, obj client.Object) error {
	annotations := obj.GetAnnotations()
	_, hasFrom := annotations[seiv1alpha1.RetainedFromAnnotation]
	_, hasReason := annotations[seiv1alpha1.RetainReasonAnnotation]
	if !hasFrom && !hasReason {
		return nil
	}
	patch := client.MergeFrom(obj.DeepCopyObject().(client.Object))
	delete(annotations, seiv1alpha1.RetainedFromAnnotation)
	delete(annotations, seiv1alpha1.RetainReasonAnnotation)
	obj.SetAnnotations(annotations)
	return r.Patch(ctx, obj, patch)
}
