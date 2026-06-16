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

// reconcileSeiNodes ensures the desired child SeiNodes exist, populates
// IncumbentNodes, and detects spec changes that require orchestration.
// Mutations are skipped while a plan is in progress or while paused.
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

	if err := r.populateIncumbentNodes(ctx, network); err != nil {
		return err
	}

	r.detectDeploymentNeeded(network)
	return nil
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

// detectDeploymentNeeded checks if deployment-worthy fields have changed
// by comparing the current template hash against the stored hash. Only
// fields that require new nodes (image, chainId) are hashed; sidecar,
// overrides, and replica changes propagate in-place.
func (r *SeiNetworkReconciler) detectDeploymentNeeded(network *seiv1alpha1.SeiNetwork) {
	if network.Status.TemplateHash == "" {
		return // first reconcile, no baseline to compare against
	}
	if len(network.Status.IncumbentNodes) == 0 {
		// No incumbent nodes (e.g. missing owner references after a manual
		// edit); a rollout with empty node lists would create a plan whose
		// tasks all complete as no-ops. Wait until populateIncumbentNodes
		// finds the child set before declaring a rollout.
		return
	}

	currentHash := templateHash(&network.Spec.Template.Spec)
	if currentHash == network.Status.TemplateHash {
		return // no deployment-worthy fields changed
	}

	// Supersession: if the spec moved since the active rollout was created,
	// replace the stale plan so the controller converges on the latest spec.
	if hasConditionTrue(network, seiv1alpha1.ConditionRolloutInProgress) {
		if network.Status.Rollout != nil && network.Status.Rollout.TargetHash == currentHash {
			return // rollout already targets the current spec
		}
		oldTarget := ""
		if network.Status.Rollout != nil {
			oldTarget = network.Status.Rollout.TargetHash
		}
		network.Status.Plan = nil
		r.Recorder.Eventf(network, corev1.EventTypeNormal, "RolloutSuperseded",
			"Spec changed during active rollout, replacing plan (old target: %s)", oldTarget)
	}

	if !hasConditionTrue(network, seiv1alpha1.ConditionRolloutInProgress) &&
		hasConditionTrue(network, seiv1alpha1.ConditionPlanInProgress) {
		return // non-deployment plan in progress (e.g. genesis)
	}

	network.Status.Rollout = &seiv1alpha1.RolloutStatus{
		TargetHash: currentHash,
		StartedAt:  metav1.Now(),
	}

	setCondition(network, seiv1alpha1.ConditionRolloutInProgress, metav1.ConditionTrue,
		"TemplateChanged", fmt.Sprintf("templateHash changed from %s to %s", network.Status.TemplateHash, currentHash))

	r.Recorder.Eventf(network, corev1.EventTypeNormal, "RolloutStarted",
		"InPlace rollout started (target: %s)", currentHash[:8])
}

// populateIncumbentNodes lists child SeiNodes and records their names
// on the network status.
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
	// Peers are intentionally in-place propagatable (templateHash godoc
	// in labels.go). Semantic.DeepEqual treats nil and empty maps/slices
	// as equal — etcd round-trips normalize empties to nil, so reflect
	// would spuriously diff after the first reconcile post-restart.
	if !equality.Semantic.DeepEqual(existing.Spec.Peers, desired.Spec.Peers) {
		existing.Spec.Peers = desired.Spec.Peers
		updated = true
	}
	if updated {
		return r.Update(ctx, existing)
	}
	return nil
}

// generateSeiNode produces the desired child SeiNode for a given ordinal.
// Pure: depends only on the SeiNetwork spec and ordinal. External
// networking (P2P external address) is GitOps-owned since PLT-451 and is
// not stamped here. The template's ExternalAddress is dropped to prevent
// every child from sharing the same value if a user set it on the template.
func generateSeiNode(network *seiv1alpha1.SeiNetwork, ordinal int) *seiv1alpha1.SeiNode {
	labels := seiNodeLabels(network, ordinal)
	annotations := seiNodeAnnotations(network)

	spec := network.Spec.Template.Spec.DeepCopy()
	spec.ExternalAddress = ""

	podLabels := make(map[string]string)
	if network.Spec.Template.Metadata != nil {
		maps.Copy(podLabels, network.Spec.Template.Metadata.Labels)
	}
	maps.Copy(podLabels, spec.PodLabels)
	podLabels[groupLabel] = network.Name
	podLabels[revisionLabel] = activeRevision(network)
	spec.PodLabels = podLabels

	// Genesis is required on a SeiNetwork and the validator role is enforced
	// by CEL; the nil-guard on Validator is defense-in-depth for in-memory
	// specs that haven't been through admission.
	gc := network.Spec.Genesis
	if spec.Validator != nil {
		if spec.ChainID == "" {
			spec.ChainID = gc.ChainID
		}
		spec.Validator.GenesisCeremony = &seiv1alpha1.GenesisCeremonyNodeConfig{
			ChainID:        gc.ChainID,
			StakingAmount:  gc.StakingAmount,
			AccountBalance: gc.AccountBalance,
			Index:          int32(ordinal),
		}
	}

	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:        seiNodeName(network, ordinal),
			Namespace:   network.Namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: *spec,
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
