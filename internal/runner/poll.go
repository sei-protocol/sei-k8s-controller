package runner

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
)

// DynamicPoller polls SeiNodeTask.status.phase via the dynamic client.
type DynamicPoller struct {
	Client dynamic.Interface
}

// Poll re-reads the SeiNodeTask until phase is Complete or Failed, or the
// context is cancelled. The returned obj is the most recent observation,
// suitable for jsonpath extraction. failureReason is populated when phase=Failed
// (from .status.task.err) so callers can surface it on exit-1.
func (p DynamicPoller) Poll(ctx context.Context, namespace, name string, interval time.Duration) (string, map[string]any, string, error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	// Read once before sleeping so a fast-completing task isn't blocked on the
	// first interval.
	for {
		obj, err := p.Client.Resource(SeiNodeTaskGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if !apierrors.IsNotFound(err) {
				return "", nil, "", fmt.Errorf("get SeiNodeTask %s/%s: %w", namespace, name, err)
			}
			// Not-found is expected briefly after apply on slow caches; keep polling.
		} else {
			phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
			switch phase {
			case PhaseComplete, PhaseFailed:
				reason := ""
				if phase == PhaseFailed {
					reason, _, _ = unstructured.NestedString(obj.Object, "status", "task", "err")
					if reason == "" {
						reason = "task reached Failed phase (no error message)"
					}
				}
				return phase, obj.Object, reason, nil
			}
		}

		select {
		case <-ctx.Done():
			return "", nil, "", fmt.Errorf("timeout waiting for %s/%s to reach terminal phase: %w", namespace, name, ctx.Err())
		case <-ticker.C:
		}
	}
}
