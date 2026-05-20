package taskruntime

import (
	"errors"
	"testing"
)

func TestRoleScoped(t *testing.T) {
	cases := []struct {
		role string
		key  VarKey
		want VarKey
	}{
		{"validator", KeyTendermintRPC, "VALIDATOR_TM_RPC"},
		{"rpc", KeyEVMJSONRPC, "RPC_EVM_RPC"},
		{"Validator", KeyChainID, "VALIDATOR_CHAIN_ID"},
	}
	for _, tc := range cases {
		t.Run(string(tc.want), func(t *testing.T) {
			if got := RoleScoped(tc.role, tc.key); got != tc.want {
				t.Fatalf("RoleScoped(%q, %q) = %q, want %q", tc.role, tc.key, got, tc.want)
			}
		})
	}
}

func TestExitReasonFor(t *testing.T) {
	plain := errors.New("plain")
	cases := []struct {
		name string
		err  error
		want ExitReason
	}{
		{"nil", nil, ExitReasonPass},
		{"plain", plain, ExitReasonTaskFail},
		{"task", Task(plain), ExitReasonTaskFail},
		{"infra", Infra(plain), ExitReasonInfraFail},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExitReasonFor(tc.err); got != tc.want {
				t.Fatalf("ExitReasonFor(%v) = %s, want %s", tc.err, got, tc.want)
			}
		})
	}
}
