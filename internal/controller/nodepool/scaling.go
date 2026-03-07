package nodepool

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

func (r *SeiNodePoolReconciler) scaleDown(ctx context.Context, sn *seiv1alpha1.SeiNodePool) error {
	nodeList := &seiv1alpha1.SeiNodeList{}
	if err := r.List(ctx, nodeList,
		client.InNamespace(sn.Namespace),
		client.MatchingLabels(resourceLabels(sn)),
	); err != nil {
		return fmt.Errorf("listing SeiNodes for scale-down: %w", err)
	}

	desired := int(sn.Spec.NodeConfiguration.NodeCount)
	for i := range nodeList.Items {
		node := &nodeList.Items[i]
		ordinal, ok := nodeOrdinal(sn, node)
		if !ok || ordinal < desired {
			continue
		}
		if err := r.Delete(ctx, node); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting excess SeiNode %s: %w", node.Name, err)
		}
	}
	return nil
}

func nodeOrdinal(sn *seiv1alpha1.SeiNodePool, node *seiv1alpha1.SeiNode) (int, bool) {
	prefix := sn.Name + "-"
	if !strings.HasPrefix(node.Name, prefix) {
		return 0, false
	}
	suffix := node.Name[len(prefix):]
	var ordinal int
	if _, err := fmt.Sscanf(suffix, "%d", &ordinal); err != nil {
		return 0, false
	}
	return ordinal, true
}
