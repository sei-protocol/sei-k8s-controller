package taskruntime

import "strings"

// VarKey is a typed key for the workflow-vars ConfigMap. Producers and
// consumers reference these constants so renames are compile errors. Schema +
// stability discipline: https://github.com/sei-protocol/bdchatham-designs/blob/main/designs/test-harness/test-harness-lld.md.
type VarKey string

const (
	// KeyRunID — Workflow CR's metadata.name. Written by the initializing Task.
	KeyRunID VarKey = "RUN_ID"

	// KeyChainID — the SeiNetwork's chainId. One-way door.
	KeyChainID VarKey = "CHAIN_ID"

	// Endpoints — written by provision-snd after the SeiNetwork is Ready.
	// KeyEVMJSONRPC is pod-0 only (release-test pins stateful EVM
	// sequences to one pod). KeyEVMJSONRPCList is comma-separated
	// per-pod URLs for seiload, whose stateful EVM workload needs to
	// hit all RPC pods.
	KeyTendermintRPC  VarKey = "TM_RPC"
	KeyTendermintREST VarKey = "REST"
	KeyEVMJSONRPC     VarKey = "EVM_RPC"
	KeyEVMJSONRPCList VarKey = "EVM_RPC_LIST"

	// Admin identity — written by keygen. Mnemonic itself lives in the
	// referenced Secret, not the ConfigMap.
	KeyAdminAddress    VarKey = "ADMIN_ADDRESS"
	KeyAdminSecretName VarKey = "ADMIN_SECRET_NAME"

	// KeyExitReason — written by the failing Task pre-exit. upload-report
	// reads this to recover the exit-code class Chaos Mesh collapses.
	KeyExitReason VarKey = "EXIT_REASON"
)

// ExitReason is the string mirror of ExitCodeFor for the EXIT_REASON CM value.
type ExitReason string

const (
	ExitReasonPass      ExitReason = "pass"
	ExitReasonTaskFail  ExitReason = "task-fail"
	ExitReasonInfraFail ExitReason = "infra-fail"
)

// ExitReasonFor mirrors ExitCodeFor: nil → pass, InfraError → infra-fail,
// otherwise → task-fail.
func ExitReasonFor(err error) ExitReason {
	switch ExitCodeFor(err) {
	case ExitPass:
		return ExitReasonPass
	case ExitInfraError:
		return ExitReasonInfraFail
	default:
		return ExitReasonTaskFail
	}
}

// RoleScoped prefixes a key with an upper-cased role tag so scenarios with
// multiple SeiNetworks (validator + rpc) write disjoint workflow-vars keys.
// RoleScoped("validator", KeyTendermintRPC) → "VALIDATOR_TM_RPC".
func RoleScoped(role string, key VarKey) VarKey {
	return VarKey(strings.ToUpper(role) + "_" + string(key))
}
