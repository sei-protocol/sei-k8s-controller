package nodedeployment

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
func (r *SeiNodeDeploymentReconciler) reconcileSeiNodes(ctx context.Context, group *seiv1alpha1.SeiNodeDeployment) error {
	if group.Spec.Paused {
		return r.populateIncumbentNodes(ctx, group)
	}

	if !hasConditionTrue(group, seiv1alpha1.ConditionPlanInProgress) {
		for i := range int(group.Spec.Replicas) {
			if err := r.ensureSeiNode(ctx, group, i); err != nil {
				return fmt.Errorf("ensuring SeiNode %d: %w", i, err)
			}
		}
		if err := r.scaleDown(ctx, group); err != nil {
			return err
		}
	} else {
		log.FromContext(ctx).Info("plan in progress, skipping SeiNode mutations")
	}

	if err := r.populateIncumbentNodes(ctx, group); err != nil {
		return err
	}

	r.detectDeploymentNeeded(group)
	return nil
}

// syncPausedToChildren brings every owned child SeiNode's Spec.Paused
// in line with desired. Children already in sync are left untouched.
func (r *SeiNodeDeploymentReconciler) syncPausedToChildren(ctx context.Context, group *seiv1alpha1.SeiNodeDeployment, desired bool) error {
	children, err := r.listChildSeiNodes(ctx, group)
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
		r.Recorder.Event(group, corev1.EventTypeNormal, action, message)
	}
	return nil
}

// setGenesisCeremonyCondition stamps ConditionGenesisCeremonyComplete
// with the SND's current genesis lifecycle state:
//
//   - True / Complete         — ceremony finished (latched)
//   - False / NotApplicable   — spec.genesis is unset
//   - False / NotStarted      — spec.genesis set, ceremony not yet started
//   - False / InProgress      — ceremony executing under an active plan
//
// The latch check runs first. Reordering it would let a completed
// ceremony be downgraded to NotApplicable if spec.genesis is cleared.
func (r *SeiNodeDeploymentReconciler) setGenesisCeremonyCondition(group *seiv1alpha1.SeiNodeDeployment) {
	if hasConditionTrue(group, seiv1alpha1.ConditionGenesisCeremonyComplete) {
		return
	}
	if group.Spec.Genesis == nil {
		setCondition(group, seiv1alpha1.ConditionGenesisCeremonyComplete, metav1.ConditionFalse,
			"NotApplicable", "spec.genesis is unset")
		return
	}
	if hasConditionTrue(group, seiv1alpha1.ConditionPlanInProgress) {
		setCondition(group, seiv1alpha1.ConditionGenesisCeremonyComplete, metav1.ConditionFalse,
			"InProgress", "genesis ceremony is executing under an active plan")
		return
	}
	setCondition(group, seiv1alpha1.ConditionGenesisCeremonyComplete, metav1.ConditionFalse,
		ReasonNotStarted, "genesis ceremony has not yet started")
}

// detectDeploymentNeeded checks if deployment-worthy fields have changed
// by comparing the current template hash against the stored hash. Only
// fields that require new nodes (image, chainId) are hashed; sidecar,
// overrides, and replica changes propagate in-place.
func (r *SeiNodeDeploymentReconciler) detectDeploymentNeeded(group *seiv1alpha1.SeiNodeDeployment) {
	if group.Status.TemplateHash == "" {
		return // first reconcile, no baseline to compare against
	}
	if len(group.Status.IncumbentNodes) == 0 {
		// No incumbent nodes (e.g. missing owner references after a manual
		// edit); a rollout with empty node lists would create a plan whose
		// tasks all complete as no-ops. Wait until populateIncumbentNodes
		// finds the child set before declaring a rollout.
		return
	}

	currentHash := templateHash(&group.Spec.Template.Spec)
	if currentHash == group.Status.TemplateHash {
		return // no deployment-worthy fields changed
	}

	// Supersession: if the spec moved since the active rollout was created,
	// replace the stale plan so the controller converges on the latest spec.
	if hasConditionTrue(group, seiv1alpha1.ConditionRolloutInProgress) {
		if group.Status.Rollout != nil && group.Status.Rollout.TargetHash == currentHash {
			return // rollout already targets the current spec
		}
		oldTarget := ""
		if group.Status.Rollout != nil {
			oldTarget = group.Status.Rollout.TargetHash
		}
		group.Status.Plan = nil
		r.Recorder.Eventf(group, corev1.EventTypeNormal, "RolloutSuperseded",
			"Spec changed during active rollout, replacing plan (old target: %s)", oldTarget)
	}

	if !hasConditionTrue(group, seiv1alpha1.ConditionRolloutInProgress) &&
		hasConditionTrue(group, seiv1alpha1.ConditionPlanInProgress) {
		return // non-deployment plan in progress (e.g. genesis)
	}

	if group.Spec.UpdateStrategy.Type == "" {
		log.Log.Info("updateStrategy.type is empty, treating as InPlace — update the manifest",
			"group", group.Name, "namespace", group.Namespace)
	}

	group.Status.Rollout = &seiv1alpha1.RolloutStatus{
		TargetHash: currentHash,
		StartedAt:  metav1.Now(),
	}

	setCondition(group, seiv1alpha1.ConditionRolloutInProgress, metav1.ConditionTrue,
		"TemplateChanged", fmt.Sprintf("templateHash changed from %s to %s", group.Status.TemplateHash, currentHash))

	r.Recorder.Eventf(group, corev1.EventTypeNormal, "RolloutStarted",
		"InPlace rollout started (target: %s)", currentHash[:8])
}

// populateIncumbentNodes lists child SeiNodes and records their names
// on the group status.
func (r *SeiNodeDeploymentReconciler) populateIncumbentNodes(ctx context.Context, group *seiv1alpha1.SeiNodeDeployment) error {
	nodes, err := r.listChildSeiNodes(ctx, group)
	if err != nil {
		return fmt.Errorf("listing child SeiNodes: %w", err)
	}
	names := make([]string, 0, len(nodes))
	for i := range nodes {
		names = append(names, nodes[i].Name)
	}
	group.Status.IncumbentNodes = names
	return nil
}

func (r *SeiNodeDeploymentReconciler) ensureSeiNode(ctx context.Context, group *seiv1alpha1.SeiNodeDeployment, ordinal int) error {
	desired := generateSeiNode(group, ordinal)
	desired.Spec.ExternalAddress = r.p2pEndpointAddressForChild(group, ordinal)
	if err := ctrl.SetControllerReference(group, desired, r.Scheme); err != nil {
		return fmt.Errorf("setting owner reference: %w", err)
	}

	existing := &seiv1alpha1.SeiNode{}
	err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if apierrors.IsNotFound(err) {
		if createErr := r.Create(ctx, desired); createErr != nil {
			return createErr
		}
		r.Recorder.Eventf(group, corev1.EventTypeNormal, "SeiNodeCreated", "Created SeiNode %s", desired.Name)
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
	if existing.Spec.ExternalAddress != desired.Spec.ExternalAddress {
		existing.Spec.ExternalAddress = desired.Spec.ExternalAddress
		updated = true
	}
	if updated {
		return r.Update(ctx, existing)
	}
	return nil
}

// generateSeiNode produces the desired child SeiNode for a given ordinal.
// Pure: depends only on the SND spec and ordinal. The publishable
// external address is injected by ensureSeiNode, which needs reconciler
// state. The template's ExternalAddress is dropped to prevent every
// child from sharing the same value if a user set it on the template.
func generateSeiNode(group *seiv1alpha1.SeiNodeDeployment, ordinal int) *seiv1alpha1.SeiNode {
	labels := seiNodeLabels(group, ordinal)
	annotations := seiNodeAnnotations(group)

	spec := group.Spec.Template.Spec.DeepCopy()
	spec.ExternalAddress = ""

	podLabels := make(map[string]string)
	if group.Spec.Template.Metadata != nil {
		maps.Copy(podLabels, group.Spec.Template.Metadata.Labels)
	}
	maps.Copy(podLabels, spec.PodLabels)
	podLabels[groupLabel] = group.Name
	podLabels[revisionLabel] = activeRevision(group)
	spec.PodLabels = podLabels

	if gc := group.Spec.Genesis; gc != nil && spec.Validator != nil {
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
			Name:        seiNodeName(group, ordinal),
			Namespace:   group.Namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: *spec,
	}
}

// p2pEndpointAddressForChild returns the vanity host:port when the
// SND opts into TCP networking, "" otherwise.
func (r *SeiNodeDeploymentReconciler) p2pEndpointAddressForChild(group *seiv1alpha1.SeiNodeDeployment, ordinal int) string {
	if !group.Spec.Networking.TCPEnabled() {
		return ""
	}
	return r.p2pEndpointAddress(group, ordinal)
}

// scaleDown deletes SeiNodes with ordinals >= the desired replica count.
func (r *SeiNodeDeploymentReconciler) scaleDown(ctx context.Context, group *seiv1alpha1.SeiNodeDeployment) error {
	if group.Spec.Replicas <= 0 {
		log.FromContext(ctx).Info("refusing scale-down: desired replicas is zero or negative")
		return nil
	}

	nodeList := &seiv1alpha1.SeiNodeList{}
	if err := r.List(ctx, nodeList,
		client.InNamespace(group.Namespace),
		client.MatchingLabels(groupSelector(group)),
	); err != nil {
		return fmt.Errorf("listing child SeiNodes: %w", err)
	}

	for i := range nodeList.Items {
		node := &nodeList.Items[i]
		if !metav1.IsControlledBy(node, group) {
			continue
		}
		if node.Labels[groupOrdinalLabel] == "" {
			continue
		}
		var ord int
		if _, err := fmt.Sscanf(node.Labels[groupOrdinalLabel], "%d", &ord); err != nil {
			continue
		}
		if ord >= int(group.Spec.Replicas) {
			// Delete the per-ordinal P2P endpoint Service before the
			// child so the NLB is not stranded between scale-down and
			// SND delete. Idempotent.
			if err := r.deleteP2PEndpointForOrdinal(ctx, group, ord); err != nil {
				return fmt.Errorf("deleting P2P endpoint Service for scaled-down ordinal %d: %w", ord, err)
			}
			if err := r.Delete(ctx, node); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("deleting excess SeiNode %s: %w", node.Name, err)
			}
			r.Recorder.Eventf(group, corev1.EventTypeNormal, "SeiNodeDeleted", "Scaled down SeiNode %s", node.Name)
		}
	}
	return nil
}

func (r *SeiNodeDeploymentReconciler) listChildSeiNodes(ctx context.Context, group *seiv1alpha1.SeiNodeDeployment) ([]seiv1alpha1.SeiNode, error) {
	nodeList := &seiv1alpha1.SeiNodeList{}
	if err := r.List(ctx, nodeList,
		client.InNamespace(group.Namespace),
		client.MatchingLabels(groupSelector(group)),
	); err != nil {
		return nil, fmt.Errorf("listing child SeiNodes: %w", err)
	}
	owned := nodeList.Items[:0]
	for i := range nodeList.Items {
		if metav1.IsControlledBy(&nodeList.Items[i], group) {
			owned = append(owned, nodeList.Items[i])
		}
	}
	return owned, nil
}
