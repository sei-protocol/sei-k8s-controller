package task

import (
	"encoding/json"
	"errors"
	"testing"

	sidecar "github.com/sei-protocol/seictl/sidecar/client"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const tpKey = "TimeoutParams"

func govParamChangeCR(value string) *seiv1alpha1.SeiNodeTask {
	return &seiv1alpha1.SeiNodeTask{
		Spec: seiv1alpha1.SeiNodeTaskSpec{
			Kind: seiv1alpha1.SeiNodeTaskKindGovParamChange,
			GovParamChange: &seiv1alpha1.GovParamChangePayload{
				ChainID:     "arctic-1",
				KeyName:     "node_admin",
				Title:       "Update Consensus Timeout Params",
				Description: "Tighten timeouts.",
				Changes: []seiv1alpha1.GovParamChangeEntry{
					{Subspace: "baseapp", Key: tpKey, Value: apiextensionsv1.JSON{Raw: []byte(value)}},
				},
				InitialDeposit: "10000000usei",
				Fees:           "8000usei",
				Gas:            300000,
			},
		},
	}
}

// kind=GovParamChange maps to the sidecar gov-param-change task with the
// proposal fields and a per-change value forwarded as RAW bytes (the single
// string() conversion happens in the sidecar, not here).
func TestSeiNodeTaskParamsFor_GovParamChange(t *testing.T) {
	cr := govParamChangeCR(`{"propose":"300000000"}`)

	p, err := SeiNodeTaskParamsFor(cr, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Type != sidecar.TaskTypeGovParamChange {
		t.Errorf("Type = %q, want %q", p.Type, sidecar.TaskTypeGovParamChange)
	}
	task, ok := p.Payload.(sidecar.GovParamChangeTask)
	if !ok {
		t.Fatalf("Payload = %T, want sidecar.GovParamChangeTask", p.Payload)
	}
	if task.ChainID != "arctic-1" || task.KeyName != "node_admin" || task.Gas != 300000 {
		t.Errorf("scalar fields not forwarded: %+v", task)
	}
	if len(task.Changes) != 1 || task.Changes[0].Subspace != "baseapp" || task.Changes[0].Key != tpKey {
		t.Fatalf("changes not mapped: %+v", task.Changes)
	}
	// Validate() must pass on the produced task (the sidecar runs it next).
	if err := task.Validate(); err != nil {
		t.Errorf("produced task fails Validate(): %v", err)
	}
}

// Regression guard for the prop-252 double-encode bug at the controller
// boundary: the CRD value (apiextensionsv1.JSON) must reach the sidecar task as
// raw bytes, byte-identical, for any JSON shape — never re-escaped/re-marshaled.
func TestSeiNodeTaskParamsFor_GovParamChange_ValueSingleEncoded(t *testing.T) {
	for _, raw := range []string{
		`{"propose":"300000000","commit":"200000000"}`, // object
		`"86400000000000"`, // scalar string
		`100`,              // scalar number
		`true`,             // scalar bool
	} {
		t.Run(raw, func(t *testing.T) {
			p, err := SeiNodeTaskParamsFor(govParamChangeCR(raw), nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			task := p.Payload.(sidecar.GovParamChangeTask)
			if got := string(task.Changes[0].Value); got != raw {
				t.Errorf("forwarded value = %q, want %q (re-encoded?)", got, raw)
			}
		})
	}
}

// keyName omitted resolves via the target's operator keyring (node_admin when
// the secret is set without an explicit keyName).
func TestSeiNodeTaskParamsFor_GovParamChange_KeyNameDerivedFromTarget(t *testing.T) {
	cr := govParamChangeCR(`{"propose":"300000000"}`)
	cr.Spec.GovParamChange.KeyName = ""
	target := &seiv1alpha1.SeiNode{
		Spec: seiv1alpha1.SeiNodeSpec{
			Validator: &seiv1alpha1.ValidatorSpec{
				OperatorKeyring: &seiv1alpha1.OperatorKeyringSource{
					Secret: &seiv1alpha1.SecretOperatorKeyringSource{},
				},
			},
		},
	}
	p, err := SeiNodeTaskParamsFor(cr, target)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := p.Payload.(sidecar.GovParamChangeTask).KeyName; got != seiv1alpha1.DefaultOperatorKeyName {
		t.Errorf("derived KeyName = %q, want %q", got, seiv1alpha1.DefaultOperatorKeyName)
	}
}

// The Type that the build path produces MUST be in the Deserialize registry,
// or the controller fails every GovParamChange task with DeserializeFailed
// before it ever reaches the sidecar. This round-trip guards the registry
// wiring (the build path alone does not exercise it).
func TestSeiNodeTaskParamsFor_GovParamChange_DeserializeRoundTrip(t *testing.T) {
	p, err := SeiNodeTaskParamsFor(govParamChangeCR(`{"propose":"300000000"}`), nil)
	if err != nil {
		t.Fatalf("params: %v", err)
	}
	raw, err := json.Marshal(p.Payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	exec, err := Deserialize(p.Type, "gpc-1", raw, ExecutionConfig{})
	if err != nil {
		t.Fatalf("Deserialize(%q) failed — is the task type registered? %v", p.Type, err)
	}
	if exec == nil {
		t.Fatal("Deserialize returned nil TaskExecution")
	}
}

// Multiple changes are forwarded in order, each value byte-preserved.
func TestSeiNodeTaskParamsFor_GovParamChange_MultipleChanges(t *testing.T) {
	cr := govParamChangeCR(`{"propose":"300000000"}`)
	cr.Spec.GovParamChange.Changes = []seiv1alpha1.GovParamChangeEntry{
		{Subspace: "baseapp", Key: tpKey, Value: apiextensionsv1.JSON{Raw: []byte(`{"propose":"300000000"}`)}},
		{Subspace: "staking", Key: "MaxValidators", Value: apiextensionsv1.JSON{Raw: []byte(`"100"`)}},
	}
	p, err := SeiNodeTaskParamsFor(cr, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	task := p.Payload.(sidecar.GovParamChangeTask)
	if len(task.Changes) != 2 {
		t.Fatalf("got %d changes, want 2", len(task.Changes))
	}
	if task.Changes[0].Key != tpKey || task.Changes[1].Key != "MaxValidators" {
		t.Errorf("order not preserved: %q, %q", task.Changes[0].Key, task.Changes[1].Key)
	}
	if string(task.Changes[1].Value) != `"100"` {
		t.Errorf("second value = %q, want %q", string(task.Changes[1].Value), `"100"`)
	}
}

// kind=GovParamChange with a nil payload is a param-build failure
// (ParamsBuildFailed), not an unsupported kind. CEL normally blocks this.
func TestSeiNodeTaskParamsFor_GovParamChange_NilPayload_ParamsBuildFailed(t *testing.T) {
	cr := &seiv1alpha1.SeiNodeTask{
		Spec: seiv1alpha1.SeiNodeTaskSpec{Kind: seiv1alpha1.SeiNodeTaskKindGovParamChange},
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
		Spec: seiv1alpha1.SeiNodeTaskSpec{Kind: seiv1alpha1.SeiNodeTaskKindRestartSeid},
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

// kind=RestartSeid maps to the sidecar restart-seid task with an empty payload —
// no target needed, no source building (mirrors MarkReady).
func TestSeiNodeTaskParamsFor_RestartSeid(t *testing.T) {
	cr := &seiv1alpha1.SeiNodeTask{
		Spec: seiv1alpha1.SeiNodeTaskSpec{
			Kind:        seiv1alpha1.SeiNodeTaskKindRestartSeid,
			RestartSeid: &seiv1alpha1.RestartSeidPayload{},
		},
	}

	p, err := SeiNodeTaskParamsFor(cr, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Type != sidecar.TaskTypeRestartSeid {
		t.Errorf("Type = %q, want %q", p.Type, sidecar.TaskTypeRestartSeid)
	}
	if _, ok := p.Payload.(sidecar.RestartSeidTask); !ok {
		t.Errorf("Payload = %T, want sidecar.RestartSeidTask", p.Payload)
	}
}

// kind=RestartSeid with a nil payload is a param-build failure (ParamsBuildFailed),
// not an unsupported kind. CEL normally blocks this; the guard is the backstop.
func TestSeiNodeTaskParamsFor_RestartSeid_NilPayload_ParamsBuildFailed(t *testing.T) {
	cr := &seiv1alpha1.SeiNodeTask{
		Spec: seiv1alpha1.SeiNodeTaskSpec{Kind: seiv1alpha1.SeiNodeTaskKindRestartSeid},
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

// FailureReason carries the reason intrinsically: a wired-kind param failure
// reports ParamsBuildFailed, an unwired kind reports UnsupportedKind. Both are
// the public condition enum (CLAUDE.md); the controller maps err→reason with no
// conditional.
func TestFailureReason(t *testing.T) {
	wired := &seiv1alpha1.SeiNodeTask{
		Spec: seiv1alpha1.SeiNodeTaskSpec{Kind: seiv1alpha1.SeiNodeTaskKindRestartSeid},
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
