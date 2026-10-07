package planner

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

// Drift-roll budget (PLT-1399). A pod-template drift (image, sidecar image,
// isolation) otherwise makes every affected node in a namespace update at
// once. With platform DriftUpdateBudgetPercent set, a namespace gets
// max(1, N*percent/100) roll slots, where N counts its Running, unpaused
// SeiNodes. Nodes already updating hold the first slots; drifted nodes waiting
// for one follow in name order. Every node computes the same order from the
// same cache, so a stale read delays a roll rather than overfilling the slots.
// A node without a slot keeps its current pod and reports UpdateDeferred.

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
	} else if len(updating) < slots {
		// The cache does not list this node as drifted yet; fall back to the
		// count of updates in progress.
		return true, "", nil
	}
	holders := order[:min(slots, len(order))]
	return false, fmt.Sprintf("drift update waits for a roll slot in namespace %s: %d slot(s), held by %s",
		node.Namespace, slots, strings.Join(holders, ", ")), nil
}

// DeferStatefulSetApply reports whether the reconciler must skip this
// reconcile's StatefulSet apply to keep a drift waiting. Only a nodeConfig
// node needs it: its StatefulSet is RollingUpdate, so applying the drifted
// template would roll the pod with no plan. It applies only when the next
// plan would be the drift update: no plan is active, and no reset or hold
// change is pending.
func (p *NodeResolver) DeferStatefulSetApply(ctx context.Context, node *seiv1alpha1.SeiNode) (bool, error) {
	if node.Spec.NodeConfig == nil || node.Status.Phase != seiv1alpha1.PhaseRunning {
		return false, nil
	}
	if node.Status.Plan != nil && node.Status.Plan.Phase == seiv1alpha1.TaskPlanActive {
		return false, nil
	}
	if !podTemplateDrifted(node, p.Platform) || task.DataResetPending(node) ||
		node.Spec.HoldRequested() != node.Status.MaintenanceHold {
		return false, nil
	}
	free, msg, err := p.DriftSlot(ctx, node)
	if err != nil {
		return true, err
	}
	if !free {
		setNodeUpdateCondition(node, metav1.ConditionFalse, reasonUpdateDeferred, msg)
	}
	return !free, nil
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
