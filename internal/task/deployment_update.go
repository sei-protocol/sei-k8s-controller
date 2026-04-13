package task

import (
	"context"
	"encoding/json"
	"fmt"

	sidecar "github.com/sei-protocol/seictl/sidecar/client"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// --- UpdateNodeSpecs: patches child SeiNode specs (image) ---

type updateNodeSpecsExecution struct {
	taskBase
	params UpdateNodeSpecsParams
	cfg    ExecutionConfig
}

func deserializeUpdateNodeSpecs(id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	var p UpdateNodeSpecsParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("deserializing update-node-specs params: %w", err)
		}
	}
	return &updateNodeSpecsExecution{
		taskBase: taskBase{id: id, status: ExecutionRunning},
		params:   p,
		cfg:      cfg,
	}, nil
}

func (e *updateNodeSpecsExecution) Execute(ctx context.Context) error {
	logger := log.FromContext(ctx)

	group, err := ResourceAs[*seiv1alpha1.SeiNodeDeployment](e.cfg)
	if err != nil {
		return Terminal(err)
	}

	desiredImage := group.Spec.Template.Spec.Image

	for _, name := range e.params.NodeNames {
		node := &seiv1alpha1.SeiNode{}
		if err := e.cfg.KubeClient.Get(ctx, types.NamespacedName{Name: name, Namespace: e.params.Namespace}, node); err != nil {
			return fmt.Errorf("getting node %s: %w", name, err)
		}
		if node.Spec.Image == desiredImage {
			continue
		}
		node.Spec.Image = desiredImage
		if sc := group.Spec.Template.Spec.Sidecar; sc != nil && node.Spec.Sidecar != nil {
			node.Spec.Sidecar.Image = sc.Image
		}
		if err := e.cfg.KubeClient.Update(ctx, node); err != nil {
			return fmt.Errorf("updating node %s spec: %w", name, err)
		}
		logger.Info("updated node spec", "node", name, "image", desiredImage)
	}

	e.complete()
	return nil
}

func (e *updateNodeSpecsExecution) Status(_ context.Context) ExecutionStatus {
	return e.status
}

// --- AwaitSpecUpdate: waits for StatefulSet rollout to complete ---

type awaitSpecUpdateExecution struct {
	taskBase
	params AwaitSpecUpdateParams
	cfg    ExecutionConfig
}

func deserializeAwaitSpecUpdate(id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	var p AwaitSpecUpdateParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("deserializing await-spec-update params: %w", err)
		}
	}
	return &awaitSpecUpdateExecution{
		taskBase: taskBase{id: id, status: ExecutionRunning},
		params:   p,
		cfg:      cfg,
	}, nil
}

func (e *awaitSpecUpdateExecution) Execute(_ context.Context) error { return nil }

func (e *awaitSpecUpdateExecution) Status(ctx context.Context) ExecutionStatus {
	if s, done := e.isTerminal(); done {
		return s
	}
	for _, name := range e.params.NodeNames {
		node := &seiv1alpha1.SeiNode{}
		if err := e.cfg.KubeClient.Get(ctx, types.NamespacedName{Name: name, Namespace: e.params.Namespace}, node); err != nil {
			return ExecutionRunning
		}
		if node.Status.CurrentImage != node.Spec.Image {
			return ExecutionRunning
		}
	}
	e.complete()
	return ExecutionComplete
}

// --- MarkNodesReady: submits mark-ready to each node's sidecar ---

type markNodesReadyExecution struct {
	taskBase
	params MarkNodesReadyParams
	cfg    ExecutionConfig
	marked map[string]bool
}

func deserializeMarkNodesReady(id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	var p MarkNodesReadyParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("deserializing mark-nodes-ready params: %w", err)
		}
	}
	return &markNodesReadyExecution{
		taskBase: taskBase{id: id, status: ExecutionRunning},
		params:   p,
		cfg:      cfg,
		marked:   make(map[string]bool, len(p.NodeNames)),
	}, nil
}

func (e *markNodesReadyExecution) Execute(_ context.Context) error { return nil }

func (e *markNodesReadyExecution) Status(ctx context.Context) ExecutionStatus {
	if s, done := e.isTerminal(); done {
		return s
	}
	logger := log.FromContext(ctx)

	allReady := true
	for _, name := range e.params.NodeNames {
		if e.marked[name] {
			continue
		}
		node := &seiv1alpha1.SeiNode{}
		if err := e.cfg.KubeClient.Get(ctx, types.NamespacedName{Name: name, Namespace: e.params.Namespace}, node); err != nil {
			allReady = false
			continue
		}
		sc, err := sidecarClientForNode(node)
		if err != nil {
			allReady = false
			continue
		}
		resp, err := sc.Status(ctx)
		if err != nil {
			allReady = false
			continue
		}
		if resp.Status == sidecar.Ready {
			e.marked[name] = true
			continue
		}
		if _, err := sc.SubmitTask(ctx, sidecar.TaskRequest{Type: sidecar.TaskTypeMarkReady}); err != nil {
			logger.V(1).Info("mark-ready submission failed", "node", name, "error", err)
		}
		allReady = false
	}

	if allReady {
		e.complete()
		return ExecutionComplete
	}
	return ExecutionRunning
}
