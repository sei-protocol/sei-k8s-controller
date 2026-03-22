package nodegroup

import (
	"context"
	"fmt"
	"maps"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

func (r *SeiNodeGroupReconciler) reconcileSeiNodes(ctx context.Context, group *seiv1alpha1.SeiNodeGroup) error {
	for i := range int(group.Spec.Replicas) {
		if err := r.ensureSeiNode(ctx, group, i); err != nil {
			return fmt.Errorf("ensuring SeiNode %d: %w", i, err)
		}
	}
	return r.scaleDown(ctx, group)
}

func (r *SeiNodeGroupReconciler) ensureSeiNode(ctx context.Context, group *seiv1alpha1.SeiNodeGroup, ordinal int) error {
	desired := generateSeiNode(group, ordinal)
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
	if desired.Spec.Entrypoint == nil && existing.Spec.Entrypoint != nil {
		existing.Spec.Entrypoint = nil
		updated = true
	} else if desired.Spec.Entrypoint != nil && (existing.Spec.Entrypoint == nil ||
		!slices.Equal(existing.Spec.Entrypoint.Command, desired.Spec.Entrypoint.Command) ||
		!slices.Equal(existing.Spec.Entrypoint.Args, desired.Spec.Entrypoint.Args)) {
		existing.Spec.Entrypoint = desired.Spec.Entrypoint
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
	if updated {
		return r.Update(ctx, existing)
	}
	return nil
}

func generateSeiNode(group *seiv1alpha1.SeiNodeGroup, ordinal int) *seiv1alpha1.SeiNode {
	labels := seiNodeLabels(group, ordinal)
	annotations := seiNodeAnnotations(group)

	spec := group.Spec.Template.Spec.DeepCopy()
	if spec.PodLabels == nil {
		spec.PodLabels = make(map[string]string)
	}
	spec.PodLabels[groupLabel] = group.Name

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

// scaleDown deletes SeiNodes with ordinals >= the desired replica count.
func (r *SeiNodeGroupReconciler) scaleDown(ctx context.Context, group *seiv1alpha1.SeiNodeGroup) error {
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
			if err := r.Delete(ctx, node); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("deleting excess SeiNode %s: %w", node.Name, err)
			}
			r.Recorder.Eventf(group, corev1.EventTypeNormal, "SeiNodeDeleted", "Scaled down SeiNode %s", node.Name)
		}
	}
	return nil
}

func (r *SeiNodeGroupReconciler) listChildSeiNodes(ctx context.Context, group *seiv1alpha1.SeiNodeGroup) ([]seiv1alpha1.SeiNode, error) {
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
