package k8s

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"

	"github.com/sei-protocol/sei-k8s-controller/sdk/sei"
)

// pollInterval is the status re-read cadence for WaitReady. The overall budget is
// the caller's ctx — there are no spec timeout fields. A var, not a const, so
// tests can shrink it.
var pollInterval = 2 * time.Second

// networkHandle is the provider-side state behind a *sei.Network. It caches the
// last-read SeiNetwork so the endpoint getters are pure field reads off status;
// WaitReady refreshes that cache as it polls.
type networkHandle struct {
	p         *Provider
	namespace string
	name      string
	net       *seiv1alpha1.SeiNetwork
}

func (h *networkHandle) Name() string      { return h.name }
func (h *networkHandle) Namespace() string { return h.namespace }

// TendermintRPC / REST read the aggregate URLs off .status.endpoints verbatim;
// "" until the network is Ready.
func (h *networkHandle) TendermintRPC() string {
	if h.net == nil || h.net.Status.Endpoints == nil {
		return ""
	}
	return h.net.Status.Endpoints.TendermintRpc
}

func (h *networkHandle) REST() string {
	if h.net == nil || h.net.Status.Endpoints == nil {
		return ""
	}
	return h.net.Status.Endpoints.TendermintRest
}

// WaitReady blocks until the SeiNetwork reaches GroupPhaseReady and a light
// serve-probe passes, failing fast on GroupPhaseFailed. The caller's ctx is the
// budget; a deadline surfaces as context.DeadlineExceeded (sei.IsTimeout).
func (h *networkHandle) WaitReady(ctx context.Context) error {
	resource := fmt.Sprintf("SeiNetwork %s/%s", h.namespace, h.name)
	err := wait.PollUntilContextCancel(ctx, pollInterval, true, func(ctx context.Context) (bool, error) {
		net := &seiv1alpha1.SeiNetwork{}
		if err := h.p.c.Get(ctx, types.NamespacedName{Namespace: h.namespace, Name: h.name}, net); err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, fmt.Errorf("%s: reading status: %w", resource, err)
		}
		switch net.Status.Phase {
		case seiv1alpha1.GroupPhaseReady:
			h.net = net
			return true, nil
		case seiv1alpha1.GroupPhaseFailed:
			return false, fmt.Errorf("%s: reached Failed phase", resource)
		default:
			return false, nil
		}
	})
	if err != nil {
		return err
	}
	// Light serve-probe: the aggregate TM RPC answers /status once. A single
	// liveness check, not the heavy catching_up + EVM consensus gate.
	if rpc := h.TendermintRPC(); rpc != "" {
		if err := probeTendermint(ctx, h.p.httpClient, rpc, resource); err != nil {
			return err
		}
	}
	return nil
}

// Delete removes the SeiNetwork. Idempotent: a NotFound is success.
func (h *networkHandle) Delete(ctx context.Context) error {
	obj := &seiv1alpha1.SeiNetwork{ObjectMeta: metav1.ObjectMeta{Name: h.name, Namespace: h.namespace}}
	if err := h.p.c.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("SeiNetwork %s/%s: delete: %w", h.namespace, h.name, err)
	}
	return nil
}

// Object returns the cached *v1alpha1.SeiNetwork — the k8s raw-CR escape hatch.
func (h *networkHandle) Object() any { return h.net }

// nodeHandle is the provider-side state behind a *sei.Node.
type nodeHandle struct {
	p         *Provider
	namespace string
	name      string
	node      *seiv1alpha1.SeiNode
}

func (h *nodeHandle) Name() string      { return h.name }
func (h *nodeHandle) Namespace() string { return h.namespace }

// EVMRPC / TendermintRPC read the node's .status.endpoint verbatim.
func (h *nodeHandle) EVMRPC() string {
	if h.node == nil || h.node.Status.Endpoint == nil {
		return ""
	}
	return h.node.Status.Endpoint.EvmJsonRpc
}

func (h *nodeHandle) TendermintRPC() string {
	if h.node == nil || h.node.Status.Endpoint == nil {
		return ""
	}
	return h.node.Status.Endpoint.TendermintRpc
}

func (h *nodeHandle) REST() string {
	if h.node == nil || h.node.Status.Endpoint == nil {
		return ""
	}
	return h.node.Status.Endpoint.TendermintRest
}

// WaitReady blocks until the SeiNode reaches PhaseRunning and a light serve-probe
// passes, failing fast on PhaseFailed. The caller's ctx is the budget.
func (h *nodeHandle) WaitReady(ctx context.Context) error {
	resource := fmt.Sprintf("SeiNode %s/%s", h.namespace, h.name)
	err := wait.PollUntilContextCancel(ctx, pollInterval, true, func(ctx context.Context) (bool, error) {
		node := &seiv1alpha1.SeiNode{}
		if err := h.p.c.Get(ctx, types.NamespacedName{Namespace: h.namespace, Name: h.name}, node); err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, fmt.Errorf("%s: reading status: %w", resource, err)
		}
		switch node.Status.Phase {
		case seiv1alpha1.PhaseRunning:
			h.node = node
			return true, nil
		case seiv1alpha1.PhaseFailed:
			return false, fmt.Errorf("%s: reached Failed phase", resource)
		default:
			return false, nil
		}
	})
	if err != nil {
		return err
	}
	// Light serve-probe: one liveness check against whichever endpoint the node
	// serves — EVM eth_blockNumber if it has EVM, else TM /status.
	if evm := h.EVMRPC(); evm != "" {
		return probeEVM(ctx, h.p.httpClient, evm, resource)
	}
	if rpc := h.TendermintRPC(); rpc != "" {
		return probeTendermint(ctx, h.p.httpClient, rpc, resource)
	}
	return nil
}

// Delete removes the SeiNode. Idempotent: a NotFound is success.
func (h *nodeHandle) Delete(ctx context.Context) error {
	obj := &seiv1alpha1.SeiNode{ObjectMeta: metav1.ObjectMeta{Name: h.name, Namespace: h.namespace}}
	if err := h.p.c.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("SeiNode %s/%s: delete: %w", h.namespace, h.name, err)
	}
	return nil
}

// Object returns the cached *v1alpha1.SeiNode — the k8s raw-CR escape hatch.
func (h *nodeHandle) Object() any { return h.node }

// taskHandle is the provider-side state behind a *sei.Task. It caches the
// last-read SeiNodeTask; WaitComplete refreshes that cache as it polls and
// returns the translated outputs from the terminal object.
type taskHandle struct {
	p         *Provider
	namespace string
	name      string
	task      *seiv1alpha1.SeiNodeTask
}

func (h *taskHandle) Name() string      { return h.name }
func (h *taskHandle) Namespace() string { return h.namespace }

// WaitComplete blocks until the SeiNodeTask reaches Complete (returning its
// translated outputs) or fails fast on Failed (returning the task's recorded
// failure reason). The caller's ctx is the budget; a deadline surfaces as
// context.DeadlineExceeded (sei.IsTimeout).
func (h *taskHandle) WaitComplete(ctx context.Context) (*sei.TaskOutputs, error) {
	resource := fmt.Sprintf("SeiNodeTask %s/%s", h.namespace, h.name)
	err := wait.PollUntilContextCancel(ctx, pollInterval, true, func(ctx context.Context) (bool, error) {
		task := &seiv1alpha1.SeiNodeTask{}
		if err := h.p.c.Get(ctx, types.NamespacedName{Namespace: h.namespace, Name: h.name}, task); err != nil {
			// A task wait spans minutes (the chain halts at the upgrade height), so
			// a single transient read error must not collapse the whole wait — keep
			// polling on retryable errors (and NotFound, for post-create cache lag).
			// The caller's ctx remains the hard budget. Only a terminal error aborts.
			if retryableGet(err) {
				return false, nil
			}
			return false, fmt.Errorf("%s: reading status: %w", resource, err)
		}
		h.task = task
		switch task.Status.Phase {
		case seiv1alpha1.SeiNodeTaskPhaseComplete:
			return true, nil
		case seiv1alpha1.SeiNodeTaskPhaseFailed:
			return false, fmt.Errorf("%s: failed: %s", resource, taskFailureReason(task))
		default:
			return false, nil
		}
	})
	if err != nil {
		return nil, err
	}
	return translateTaskOutputs(h.task.Status.Outputs), nil
}

// Delete removes the SeiNodeTask. Idempotent: a NotFound is success.
func (h *taskHandle) Delete(ctx context.Context) error {
	obj := &seiv1alpha1.SeiNodeTask{ObjectMeta: metav1.ObjectMeta{Name: h.name, Namespace: h.namespace}}
	if err := h.p.c.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("SeiNodeTask %s/%s: delete: %w", h.namespace, h.name, err)
	}
	return nil
}

// Object returns the cached *v1alpha1.SeiNodeTask — the k8s raw-CR escape hatch.
func (h *taskHandle) Object() any { return h.task }

// retryableGet reports whether a Get error should be tolerated by continuing to
// poll rather than aborting the wait: NotFound (post-create cache lag, or a task
// briefly absent from the informer) and the transient server-side classes a
// minutes-long wait will occasionally hit. A genuinely terminal error (Forbidden,
// schema) is not retryable and aborts.
func retryableGet(err error) bool {
	return apierrors.IsNotFound(err) ||
		apierrors.IsServerTimeout(err) ||
		apierrors.IsTimeout(err) ||
		apierrors.IsTooManyRequests(err) ||
		apierrors.IsInternalError(err)
}

// taskFailureReason extracts a human reason from a failed task: the execution's
// recorded error, else a False condition message (preferring the terminal Ready
// condition over others for a stable message), else the phase itself so the
// error always carries some triage signal. The execution error is nil on a
// failure before target resolution (e.g. a RequirePhase timeout), where the
// condition message is the only source.
func taskFailureReason(task *seiv1alpha1.SeiNodeTask) string {
	if task.Status.Task != nil && task.Status.Task.Err != "" {
		return task.Status.Task.Err
	}
	var firstFalse string
	for _, c := range task.Status.Conditions {
		if c.Status != metav1.ConditionFalse || c.Message == "" {
			continue
		}
		if c.Type == seiv1alpha1.ConditionSeiNodeTaskReady {
			return c.Message
		}
		if firstFalse == "" {
			firstFalse = c.Message
		}
	}
	if firstFalse != "" {
		return firstFalse
	}
	return fmt.Sprintf("no failure reason recorded (phase=%s)", task.Status.Phase)
}

// translateTaskOutputs maps the CRD outputs to the SDK-native shape. Returns nil
// when the task recorded no outputs. Only UpdateNodeImage is surfaced — the only
// kind the controller's populateOutputs writes; gov/await kinds coordinate via
// chain queries (chain-as-medium), so the SDK exposes no always-empty fields.
func translateTaskOutputs(out *seiv1alpha1.SeiNodeTaskOutputs) *sei.TaskOutputs {
	if out == nil {
		return nil
	}
	t := &sei.TaskOutputs{}
	if o := out.UpdateNodeImage; o != nil {
		t.UpdateNodeImage = &sei.UpdateNodeImageOutputs{AppliedImage: o.AppliedImage}
	}
	if o := out.GovSoftwareUpgrade; o != nil {
		t.GovSoftwareUpgrade = &sei.GovProposalOutputs{TxHash: o.TxHash, Height: o.Height, ProposalID: o.ProposalID}
	}
	if o := out.GovParamChange; o != nil {
		t.GovParamChange = &sei.GovProposalOutputs{TxHash: o.TxHash, Height: o.Height, ProposalID: o.ProposalID}
	}
	if o := out.GovVote; o != nil {
		t.GovVote = &sei.GovVoteOutputs{TxHash: o.TxHash, Height: o.Height}
	}
	if *t == (sei.TaskOutputs{}) {
		return nil
	}
	return t
}
