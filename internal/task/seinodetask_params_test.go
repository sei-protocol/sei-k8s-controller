package task

import (
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	sidecar "github.com/sei-protocol/seictl/sidecar/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// An unwired kind (one the CRD enum may admit but this build does not dispatch)
// returns a typed *ErrUnsupportedKind carrying the offending kind, so the
// synthesis site can route it to reason=UnsupportedKind via errors.As.
func TestSeiNodeTaskParamsFor_UnsupportedKind_TypedError(t *testing.T) {
	cr := &seiv1alpha1.SeiNodeTask{
		Spec: seiv1alpha1.SeiNodeTaskSpec{Kind: seiv1alpha1.SeiNodeTaskKind("FutureUnwiredKind")},
	}

	_, err := SeiNodeTaskParamsFor(cr, nil)
	if err == nil {
		t.Fatal("expected error for unwired kind, got nil")
	}

	var unsupported *ErrUnsupportedKind
	if !errors.As(err, &unsupported) {
		t.Fatalf("expected *ErrUnsupportedKind, got %T", err)
	}
	if unsupported.Kind != seiv1alpha1.SeiNodeTaskKind("FutureUnwiredKind") {
		t.Errorf("ErrUnsupportedKind.Kind = %q, want %q", unsupported.Kind, "FutureUnwiredKind")
	}
}

// A wired kind whose payload is missing is a param-build failure, NOT an
// unsupported kind — it must not satisfy errors.As(*ErrUnsupportedKind).
func TestSeiNodeTaskParamsFor_WiredKindMissingPayload_NotUnsupported(t *testing.T) {
	cr := &seiv1alpha1.SeiNodeTask{
		Spec: seiv1alpha1.SeiNodeTaskSpec{Kind: seiv1alpha1.SeiNodeTaskKindDiscoverPeers},
	}

	_, err := SeiNodeTaskParamsFor(cr, nil)
	if err == nil {
		t.Fatal("expected error for missing payload, got nil")
	}
	var unsupported *ErrUnsupportedKind
	if errors.As(err, &unsupported) {
		t.Error("missing-payload error must not be *ErrUnsupportedKind")
	}
}

// kind=MarkReady maps to the sidecar mark-ready task with an empty payload —
// no target needed, no source building.
func TestSeiNodeTaskParamsFor_MarkReady(t *testing.T) {
	cr := &seiv1alpha1.SeiNodeTask{
		Spec: seiv1alpha1.SeiNodeTaskSpec{
			Kind:      seiv1alpha1.SeiNodeTaskKindMarkReady,
			MarkReady: &seiv1alpha1.MarkReadyPayload{},
		},
	}

	p, err := SeiNodeTaskParamsFor(cr, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Type != sidecar.TaskTypeMarkReady {
		t.Errorf("Type = %q, want %q", p.Type, sidecar.TaskTypeMarkReady)
	}
	if _, ok := p.Payload.(sidecar.MarkReadyTask); !ok {
		t.Errorf("Payload = %T, want sidecar.MarkReadyTask", p.Payload)
	}
}

// kind=MarkReady with a nil payload is a param-build failure (ParamsBuildFailed),
// not an unsupported kind. CEL normally blocks this; the guard is the backstop.
func TestSeiNodeTaskParamsFor_MarkReady_NilPayload_ParamsBuildFailed(t *testing.T) {
	cr := &seiv1alpha1.SeiNodeTask{
		Spec: seiv1alpha1.SeiNodeTaskSpec{Kind: seiv1alpha1.SeiNodeTaskKindMarkReady},
	}

	_, err := SeiNodeTaskParamsFor(cr, nil)
	if err == nil {
		t.Fatal("expected error for missing payload, got nil")
	}
	var unsupported *ErrUnsupportedKind
	if errors.As(err, &unsupported) {
		t.Error("missing-payload error must not be *ErrUnsupportedKind")
	}
	if got := FailureReason(err); got != ReasonParamsBuildFailed {
		t.Errorf("FailureReason = %q, want %q", got, ReasonParamsBuildFailed)
	}
}

// restartPodParams reads the restart UID straight from the immutable
// spec.restartPod.podUID — independent of status.task. Both the early-validation
// path (status.task nil) and the post-synthesis path (status.task set) yield the
// spec value.
func TestRestartPodParams_ReadsSpecPodUID(t *testing.T) {
	base := func() *seiv1alpha1.SeiNodeTask {
		return &seiv1alpha1.SeiNodeTask{
			Spec: seiv1alpha1.SeiNodeTaskSpec{
				Kind:       seiv1alpha1.SeiNodeTaskKindRestartPod,
				RestartPod: &seiv1alpha1.RestartPodPayload{PodUID: "pod-uid-1"},
			},
		}
	}

	cases := map[string]*seiv1alpha1.SeiNodeTask{
		"status.task nil": base(),
		"status.task set": func() *seiv1alpha1.SeiNodeTask {
			cr := base()
			cr.Status.Task = &seiv1alpha1.SeiNodeTaskExecution{ID: "task-id"}
			return cr
		}(),
	}

	for name, cr := range cases {
		t.Run(name, func(t *testing.T) {
			p, err := SeiNodeTaskParamsFor(cr, nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			rp, ok := p.Payload.(RestartPodParams)
			if !ok {
				t.Fatalf("Payload = %T, want RestartPodParams", p.Payload)
			}
			if rp.RestartedPodUID != types.UID("pod-uid-1") {
				t.Errorf("RestartedPodUID = %q, want %q", rp.RestartedPodUID, "pod-uid-1")
			}
		})
	}
}

// FailureReason carries the reason intrinsically: a wired-kind param failure
// reports ParamsBuildFailed, an unwired kind reports UnsupportedKind. Both are
// the public condition enum (CLAUDE.md); the controller maps err→reason with no
// conditional.
func TestFailureReason(t *testing.T) {
	wired := &seiv1alpha1.SeiNodeTask{
		Spec: seiv1alpha1.SeiNodeTaskSpec{Kind: seiv1alpha1.SeiNodeTaskKindDiscoverPeers},
	}
	_, err := SeiNodeTaskParamsFor(wired, nil)
	if err == nil {
		t.Fatal("expected error for missing payload, got nil")
	}
	if got := FailureReason(err); got != ReasonParamsBuildFailed {
		t.Errorf("FailureReason(wired-kind param failure) = %q, want %q", got, ReasonParamsBuildFailed)
	}

	unwired := &seiv1alpha1.SeiNodeTask{
		Spec: seiv1alpha1.SeiNodeTaskSpec{Kind: seiv1alpha1.SeiNodeTaskKind("FutureUnwiredKind")},
	}
	_, err = SeiNodeTaskParamsFor(unwired, nil)
	if err == nil {
		t.Fatal("expected error for unwired kind, got nil")
	}
	if got := FailureReason(err); got != ReasonUnsupportedKind {
		t.Errorf("FailureReason(unwired kind) = %q, want %q", got, ReasonUnsupportedKind)
	}

	// A plain error with no reason defaults to ParamsBuildFailed.
	if got := FailureReason(errors.New("bare")); got != ReasonParamsBuildFailed {
		t.Errorf("FailureReason(bare error) = %q, want %q", got, ReasonParamsBuildFailed)
	}
}
