package planner

import (
	"context"
	"fmt"
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/noderesource"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

// Drift-roll budget (PLT-1399). A pod-template drift (image, sidecar image,
// isolation) otherwise makes every affected node in a namespace update at
// once. With platform DriftUpdateBudgetPercent set, a namespace gets
// max(1, N*percent/100) roll slots, where N counts its Running, unpaused
// SeiNodes. Nodes already updating hold the first slots; drifted nodes waiting
// for one follow in name order. The list is an uncached read and the node
// controller reconciles one node at a time, so each decision sees every earlier
// node's persisted status and the slots never overfill. A node without a slot
// keeps its current pod and reports UpdateDeferred.

const reasonUpdateDeferred = "UpdateDeferred"

// DriftSlot reports whether node may start a pod-template drift update now.
// When it may not, the message names the slot holders.
func (p *NodeResolver) DriftSlot(ctx context.Context, node *seiv1alpha1.SeiNode) (bool, string, error) {
	percent := p.Platform.DriftUpdateBudgetPercent
	if percent <= 0 || p.Nodes == nil {
		return true, "", nil
	}
	var list seiv1alpha1.SeiNodeList
	if err := p.Nodes.List(ctx, &list, client.InNamespace(node.Namespace)); err != nil {
		return false, "", fmt.Errorf("listing SeiNodes for the drift-roll budget: %w", err)
	}
	var running int
	var updating, waiting []string
	for i := range list.Items {
		n := &list.Items[i]
		if n.Status.Phase != seiv1alpha1.PhaseRunning || n.Spec.Paused {
			continue
		}
		running++
		switch {
		case updateInProgress(n):
			updating = append(updating, n.Name)
		case podTemplateDrifted(n, p.Platform):
			waiting = append(waiting, n.Name)
		}
	}
	slices.Sort(updating)
	slices.Sort(waiting)
	slots := max(1, running*percent/100)
	order := append(updating, waiting...)
	if i := slices.Index(order, node.Name); i >= 0 {
		if i < slots {
			return true, "", nil
		}
	} else if len(order) < slots {
		// The list does not show this node as drifted yet; it may start only
		// if a slot is left after every node already in the order.
		return true, "", nil
	}
	holders := order[:min(slots, len(order))]
	return false, fmt.Sprintf("drift update waits for a roll slot in namespace %s: %d slot(s), held by %s",
		node.Namespace, slots, strings.Join(holders, ", ")), nil
}

// DriftRenderNode returns the SeiNode the reconciler renders the StatefulSet
// from. A nodeConfig node's StatefulSet is RollingUpdate, so applying a
// drifted template rolls the pod at once. While such a node waits for a roll
// slot, this returns a copy pinned to the running image, sidecar image, and
// isolation; every other field still renders from the node, so pause, resize,
// and config refs apply. Otherwise it returns node itself.
//
// It pins only when no plan can read the template this reconcile: no plan is
// active, and no reset or hold change is pending. observe-image stamps the
// spec image once a rollout completes, so a plan that ran on a pinned template
// would record an image the pod does not run. A paused node is not pinned:
// it runs no pod, and the next unpause renders the pinned template.
func (p *NodeResolver) DriftRenderNode(ctx context.Context, node *seiv1alpha1.SeiNode) (*seiv1alpha1.SeiNode, error) {
	if node.Spec.NodeConfig == nil || node.Spec.Paused || node.Status.Phase != seiv1alpha1.PhaseRunning {
		return node, nil
	}
	if node.Status.Plan != nil && node.Status.Plan.Phase == seiv1alpha1.TaskPlanActive {
		return node, nil
	}
	if updateInProgress(node) || !podTemplateDrifted(node, p.Platform) || task.DataResetPending(node) ||
		node.Spec.HoldRequested() != node.Status.MaintenanceHold {
		return node, nil
	}
	free, msg, err := p.DriftSlot(ctx, node)
	if err != nil {
		return node, err
	}
	if free {
		return node, nil
	}
	setNodeUpdateCondition(node, metav1.ConditionFalse, reasonUpdateDeferred, msg)
	pinned, err := p.pinnedToRunning(ctx, node)
	if err != nil || pinned == nil {
		// With no running image to pin to, the template cannot hold the pod;
		// the plan gate still defers the drift plan.
		return node, err
	}
	return pinned, nil
}

// pinnedToRunning returns a copy of node whose image, sidecar image, and
// isolation are the ones its pod runs now. Each comes from status, or, for an
// image status has not observed yet, from the live StatefulSet's template.
// It returns nil when no running seid image is known.
func (p *NodeResolver) pinnedToRunning(ctx context.Context, node *seiv1alpha1.SeiNode) (*seiv1alpha1.SeiNode, error) {
	seidImage, sidecarImage := node.Status.CurrentImage, node.Status.CurrentSidecarImage
	if seidImage == "" || sidecarImage == "" {
		sts := &appsv1.StatefulSet{}
		err := p.Nodes.Get(ctx, types.NamespacedName{Name: node.Name, Namespace: node.Namespace}, sts)
		switch {
		case err == nil:
			liveSeid, liveSidecar := noderesource.TemplateImages(sts)
			if seidImage == "" {
				seidImage = liveSeid
			}
			if sidecarImage == "" {
				sidecarImage = liveSidecar
			}
		case !apierrors.IsNotFound(err):
			return nil, fmt.Errorf("reading the live StatefulSet to pin a waiting drift: %w", err)
		}
	}
	if seidImage == "" {
		return nil, nil
	}
	pinned := node.DeepCopy()
	pinned.Spec.Image = seidImage
	if sidecarImage != "" {
		if pinned.Spec.Sidecar == nil {
			pinned.Spec.Sidecar = &seiv1alpha1.SidecarConfig{}
		}
		pinned.Spec.Sidecar.Image = sidecarImage
	}
	if iso := node.Status.CurrentNodeIsolation; iso != "" {
		if pinned.Spec.Scheduling == nil {
			pinned.Spec.Scheduling = &seiv1alpha1.SchedulingConfig{}
		}
		pinned.Spec.Scheduling.NodeIsolation = iso
	}
	return pinned, nil
}

// isDriftUpdatePlan reports whether plan is the pod-template drift update the
// budget paces. Resets, holds, and config updates are not.
func (p *NodeResolver) isDriftUpdatePlan(node *seiv1alpha1.SeiNode, plan *seiv1alpha1.TaskPlan) bool {
	return podTemplateDrifted(node, p.Platform) && classifyPlan(plan) == planClassNodeUpdate
}

func updateInProgress(n *seiv1alpha1.SeiNode) bool {
	c := meta.FindStatusCondition(n.Status.Conditions, seiv1alpha1.ConditionNodeUpdateInProgress)
	return c != nil && c.Status == metav1.ConditionTrue
}
