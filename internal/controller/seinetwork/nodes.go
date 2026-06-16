package seinetwork

import (
	"context"
	"fmt"
	"maps"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// reconcileSeiNodes ensures the desired child SeiNodes exist with the desired
// spec (image/sidecar/overrides/labels propagated in-place every reconcile)
// and refreshes IncumbentNodes for the genesis planner. Mutations are skipped
// while a plan is in progress (guarding the ceremony's child-Peers writes) or
// while paused.
func (r *SeiNetworkReconciler) reconcileSeiNodes(ctx context.Context, network *seiv1alpha1.SeiNetwork) error {
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
//   - True / Complete    — ceremony finished (latched)
//   - False / InProgress — ceremony executing under an active plan
//   - False / NotStarted — ceremony not yet started
//
// Every SeiNetwork runs the ceremony (genesis is required), so there is no
// NotApplicable branch. The latch check runs first.
func (r *SeiNetworkReconciler) setGenesisCeremonyCondition(network *seiv1alpha1.SeiNetwork) {
	if hasConditionTrue(network, seiv1alpha1.ConditionGenesisCeremonyComplete) {
		return
	}
	if hasConditionTrue(network, seiv1alpha1.ConditionPlanInProgress) {
		setCondition(network, seiv1alpha1.ConditionGenesisCeremonyComplete, metav1.ConditionFalse,
			"InProgress", "genesis ceremony is executing under an active plan")
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

	updated := false
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
	if desired.Spec.Sidecar == nil && existing.Spec.Sidecar != nil {
		existing.Spec.Sidecar = nil
		updated = true
	} else if desired.Spec.Sidecar != nil && (existing.Spec.Sidecar == nil ||
		existing.Spec.Sidecar.Image != desired.Spec.Sidecar.Image ||
		existing.Spec.Sidecar.Port != desired.Spec.Sidecar.Port) {
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
	// No identity / Peers / DataVolume sync below — deliberate, all create-time only:
	//   - Peers are controller-owned: the genesis ceremony's collect-and-set-peers
	//     task patches each child's Spec.Peers with the assembled validator set
	//     (a StaticPeerSource). generateSeiNode emits empty peers at create, so
	//     syncing here would clobber the ceremony's writes every loop.
	//   - DataVolume backs a StatefulSet volumeClaimTemplate, which is immutable
	//     post-create; a post-create spec.dataVolume edit cannot take effect, so
	//     we do not attempt to sync it.
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

	podLabels := make(map[string]string, len(network.Spec.PodLabels)+1)
	maps.Copy(podLabels, network.Spec.PodLabels)
	podLabels[groupLabel] = network.Name

	spec := seiv1alpha1.SeiNodeSpec{
		ChainID:    gc.ChainID,
		Image:      network.Spec.Image,
		Overrides:  maps.Clone(network.Spec.ConfigOverrides),
		Sidecar:    network.Spec.Sidecar.DeepCopy(),
		DataVolume: network.Spec.DataVolume.DeepCopy(),
		PodLabels:  podLabels,
		Paused:     network.Spec.Paused,
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

// scaleDown deletes SeiNodes with ordinals >= the desired replica count.
func (r *SeiNetworkReconciler) scaleDown(ctx context.Context, network *seiv1alpha1.SeiNetwork) error {
	if network.Spec.Replicas <= 0 {
		log.FromContext(ctx).Info("refusing scale-down: desired replicas is zero or negative")
		return nil
	}

	nodeList := &seiv1alpha1.SeiNodeList{}
	if err := r.List(ctx, nodeList,
		client.InNamespace(network.Namespace),
		client.MatchingLabels(groupSelector(network)),
	); err != nil {
		return fmt.Errorf("listing child SeiNodes: %w", err)
	}

	for i := range nodeList.Items {
		node := &nodeList.Items[i]
		if !metav1.IsControlledBy(node, network) {
			continue
		}
		if node.Labels[groupOrdinalLabel] == "" {
			continue
		}
		var ord int
		if _, err := fmt.Sscanf(node.Labels[groupOrdinalLabel], "%d", &ord); err != nil {
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
		client.MatchingLabels(groupSelector(network)),
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

// orphanChildSeiNodes strips the network owner ref so children survive
// SeiNetwork deletion under DeletionPolicyRetain.
func (r *SeiNetworkReconciler) orphanChildSeiNodes(ctx context.Context, network *seiv1alpha1.SeiNetwork) error {
	nodes, err := r.listChildSeiNodes(ctx, network)
	if err != nil {
		return err
	}
	for i := range nodes {
		node := &nodes[i]
		if err := r.removeOwnerRef(ctx, node, network); err != nil {
			return fmt.Errorf("orphaning SeiNode %s: %w", node.Name, err)
		}
	}
	return nil
}

// removeOwnerRef drops the network's owner reference from obj.
func (r *SeiNetworkReconciler) removeOwnerRef(ctx context.Context, obj client.Object, owner *seiv1alpha1.SeiNetwork) error {
	refs := obj.GetOwnerReferences()
	filtered := make([]metav1.OwnerReference, 0, len(refs))
	for _, ref := range refs {
		if ref.UID != owner.UID {
			filtered = append(filtered, ref)
		}
	}
	if len(filtered) == len(refs) {
		return nil
	}
	patch := client.MergeFrom(obj.DeepCopyObject().(client.Object))
	obj.SetOwnerReferences(filtered)
	return r.Patch(ctx, obj, patch)
}
