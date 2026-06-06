package task

import (
	"errors"
	"testing"

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
