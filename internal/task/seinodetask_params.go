package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	seiconfig "github.com/sei-protocol/sei-config"
	sidecar "github.com/sei-protocol/seictl/sidecar/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// SeiNodeTaskParams holds a synthesized task type and its payload. The caller
// marshals Payload itself; a nil Payload marshals to nil params.
type SeiNodeTaskParams struct {
	Type    string
	Payload any
}

// Stable SeiNodeTask Failed-condition reasons for param-build errors. These are
// a public enum (CLAUDE.md) consumed by runbooks/alerting — keep the strings
// stable.
const (
	ReasonParamsBuildFailed = "ParamsBuildFailed"
	ReasonUnsupportedKind   = "UnsupportedKind"
)

// ReasonedError is an error that carries the stable SeiNodeTask condition
// reason it should surface as. Every error SeiNodeTaskParamsFor returns
// implements it, so the synthesis/driveTask call sites read the reason off the
// error via FailureReason.
type ReasonedError interface {
	error
	Reason() string
}

// FailureReason returns the stable condition reason for a param-build error.
// It unwraps to a ReasonedError when present and otherwise defaults to
// ParamsBuildFailed for any other error (e.g. a marshal error wrapped by the
// caller).
func FailureReason(err error) string {
	var re ReasonedError
	if errors.As(err, &re) {
		return re.Reason()
	}
	return ReasonParamsBuildFailed
}

// ErrUnsupportedKind reports an unwired SeiNodeTask kind: one the CRD enum
// admits but this build leaves unwired. It carries the offending kind and
// reports reason=UnsupportedKind (a public enum, CLAUDE.md).
type ErrUnsupportedKind struct {
	Kind seiv1alpha1.SeiNodeTaskKind
}

func (e *ErrUnsupportedKind) Error() string {
	return fmt.Sprintf("unsupported kind %q is not wired in this build", e.Kind)
}

func (e *ErrUnsupportedKind) Reason() string { return ReasonUnsupportedKind }

// paramsError is a param-build failure for a wired kind (a missing payload that
// CEL normally blocks). It reports reason=ParamsBuildFailed so FailureReason
// routes it without a call-site conditional.
type paramsError struct{ msg string }

func (e *paramsError) Error() string  { return e.msg }
func (e *paramsError) Reason() string { return ReasonParamsBuildFailed }

// paramsErr builds a ParamsBuildFailed-reasoned error.
func paramsErr(msg string) error {
	return &paramsError{msg: msg}
}

// SeiNodeTaskParamsFor resolves a SeiNodeTask kind to its task type and
// payload. target is the resolved SeiNode; pass nil from the early-validation
// path (before the SeiNode is fetched) — derivation that needs target (e.g.
// gov-proposal signing identity) is deferred to the driveTask call site.
func SeiNodeTaskParamsFor(cr *seiv1alpha1.SeiNodeTask, target *seiv1alpha1.SeiNode) (SeiNodeTaskParams, error) {
	switch cr.Spec.Kind {
	case seiv1alpha1.SeiNodeTaskKindUpdateNodeImage:
		return updateNodeImageParams(cr)
	case seiv1alpha1.SeiNodeTaskKindGovVote:
		return govVoteParams(cr, target)
	case seiv1alpha1.SeiNodeTaskKindGovSoftwareUpgrade:
		return govSoftwareUpgradeParams(cr, target)
	case seiv1alpha1.SeiNodeTaskKindGovParamChange:
		return govParamChangeParams(cr, target)
	case seiv1alpha1.SeiNodeTaskKindAwaitCondition:
		return awaitConditionParams(cr)
	case seiv1alpha1.SeiNodeTaskKindAwaitNodesAtHeight:
		return awaitNodesAtHeightParams(cr)
	case seiv1alpha1.SeiNodeTaskKindRestartSeid:
		return restartSeidParams(cr)
	case seiv1alpha1.SeiNodeTaskKindMarkReady:
		return markReadyParams(cr)
	case seiv1alpha1.SeiNodeTaskKindConfigPatch:
		return configPatchParams(cr)
	default:
		return SeiNodeTaskParams{}, &ErrUnsupportedKind{Kind: cr.Spec.Kind}
	}
}

func updateNodeImageParams(cr *seiv1alpha1.SeiNodeTask) (SeiNodeTaskParams, error) {
	if cr.Spec.UpdateNodeImage == nil {
		return SeiNodeTaskParams{}, paramsErr("spec.updateNodeImage is required for kind=UpdateNodeImage")
	}
	return SeiNodeTaskParams{TaskTypeUpdateNodeImage, UpdateNodeImageParams{
		NodeName:  cr.Spec.Target.NodeRef.Name,
		Namespace: cr.Namespace,
		Image:     cr.Spec.UpdateNodeImage.Image,
	}}, nil
}

func govVoteParams(cr *seiv1alpha1.SeiNodeTask, target *seiv1alpha1.SeiNode) (SeiNodeTaskParams, error) {
	p := cr.Spec.GovVote
	if p == nil {
		return SeiNodeTaskParams{}, paramsErr("spec.govVote is required for kind=GovVote")
	}
	return SeiNodeTaskParams{sidecar.TaskTypeGovVote, sidecar.GovVoteTask{
		ChainID:    p.ChainID,
		KeyName:    resolveSigningUID(p.KeyName, target),
		ProposalID: p.ProposalID,
		Option:     p.Option,
		Memo:       p.Memo,
		Fees:       p.Fees,
		Gas:        p.Gas,
	}}, nil
}

func govSoftwareUpgradeParams(cr *seiv1alpha1.SeiNodeTask, target *seiv1alpha1.SeiNode) (SeiNodeTaskParams, error) {
	p := cr.Spec.GovSoftwareUpgrade
	if p == nil {
		return SeiNodeTaskParams{}, paramsErr("spec.govSoftwareUpgrade is required for kind=GovSoftwareUpgrade")
	}
	return SeiNodeTaskParams{sidecar.TaskTypeGovSoftwareUpgrade, sidecar.GovSoftwareUpgradeTask{
		ChainID:        p.ChainID,
		KeyName:        resolveSigningUID(p.KeyName, target),
		Title:          p.Title,
		Description:    p.Description,
		UpgradeName:    p.UpgradeName,
		UpgradeHeight:  p.UpgradeHeight,
		UpgradeInfo:    p.UpgradeInfo,
		InitialDeposit: p.InitialDeposit,
		Memo:           p.Memo,
		Fees:           p.Fees,
		Gas:            p.Gas,
	}}, nil
}

func govParamChangeParams(cr *seiv1alpha1.SeiNodeTask, target *seiv1alpha1.SeiNode) (SeiNodeTaskParams, error) {
	p := cr.Spec.GovParamChange
	if p == nil {
		return SeiNodeTaskParams{}, paramsErr("spec.govParamChange is required for kind=GovParamChange")
	}
	// Forward each change's value as raw JSON bytes. The sidecar handler does
	// the single string() conversion (gov_param_change.go) — converting here
	// would double-encode and fail at apply.
	changes := make([]sidecar.ParamChangeInput, 0, len(p.Changes))
	for _, c := range p.Changes {
		changes = append(changes, sidecar.ParamChangeInput{
			Subspace: c.Subspace,
			Key:      c.Key,
			Value:    json.RawMessage(c.Value.Raw),
		})
	}
	return SeiNodeTaskParams{sidecar.TaskTypeGovParamChange, sidecar.GovParamChangeTask{
		ChainID:        p.ChainID,
		KeyName:        resolveSigningUID(p.KeyName, target),
		Title:          p.Title,
		Description:    p.Description,
		Changes:        changes,
		InitialDeposit: p.InitialDeposit,
		Memo:           p.Memo,
		Fees:           p.Fees,
		Gas:            p.Gas,
	}}, nil
}

func awaitConditionParams(cr *seiv1alpha1.SeiNodeTask) (SeiNodeTaskParams, error) {
	p := cr.Spec.AwaitCondition
	if p == nil {
		return SeiNodeTaskParams{}, paramsErr("spec.awaitCondition is required for kind=AwaitCondition")
	}
	if p.Height == nil {
		return SeiNodeTaskParams{}, paramsErr("spec.awaitCondition.height is required (height is the only condition wired in MVP)")
	}
	return SeiNodeTaskParams{sidecar.TaskTypeAwaitCondition, sidecar.AwaitConditionTask{
		Condition:    sidecar.ConditionHeight,
		TargetHeight: p.Height.TargetHeight,
		Action:       p.Action,
	}}, nil
}

// awaitNodesAtHeightParams maps the single-node target to the sidecar's
// per-node await-condition(height=H) primitive rather than the
// deployment-scoped fan-out task, keeping status.task.id equal to the sidecar
// task ID.
func awaitNodesAtHeightParams(cr *seiv1alpha1.SeiNodeTask) (SeiNodeTaskParams, error) {
	p := cr.Spec.AwaitNodesAtHeight
	if p == nil {
		return SeiNodeTaskParams{}, paramsErr("spec.awaitNodesAtHeight is required for kind=AwaitNodesAtHeight")
	}
	return SeiNodeTaskParams{sidecar.TaskTypeAwaitCondition, sidecar.AwaitConditionTask{
		Condition:    sidecar.ConditionHeight,
		TargetHeight: p.TargetHeight,
	}}, nil
}

// restartSeidParams builds the empty sidecar restart-seid payload. CEL requires
// spec.restartSeid for kind=RestartSeid; this guard covers the early-validation
// path (taskParamsForKind runs before the spec is admission-checked in tests).
func restartSeidParams(cr *seiv1alpha1.SeiNodeTask) (SeiNodeTaskParams, error) {
	if cr.Spec.RestartSeid == nil {
		return SeiNodeTaskParams{}, paramsErr("spec.restartSeid is required for kind=RestartSeid")
	}
	return SeiNodeTaskParams{sidecar.TaskTypeRestartSeid, sidecar.RestartSeidTask{}}, nil
}

// markReadyParams builds the empty sidecar mark-ready payload. CEL requires
// spec.markReady for kind=MarkReady; this guard covers the early-validation path
// (taskParamsForKind runs before the spec is admission-checked in tests).
func markReadyParams(cr *seiv1alpha1.SeiNodeTask) (SeiNodeTaskParams, error) {
	if cr.Spec.MarkReady == nil {
		return SeiNodeTaskParams{}, paramsErr("spec.markReady is required for kind=MarkReady")
	}
	return SeiNodeTaskParams{sidecar.TaskTypeMarkReady, sidecar.MarkReadyTask{}}, nil
}

// configPatchParams synthesizes an incremental config-apply sidecar task from
// the payload's dotted sei-config overrides. sei-config's own resolver on the
// node (ResolveIncrementalIntent -> registry-aware ApplyOverrides -> the legacy
// renderer) turns the overrides into on-disk config, so the controller forwards
// only the dotted overrides — that path is correct-by-construction for every
// registered key. Mode is left empty: incremental resolution reads the node's
// current on-disk config and patches it, so no mode default is needed.
//
// The controller still enforces the runtime-safe ALLOWLIST as pure policy —
// sei-config's registry only checks that a key exists and type-checks its value,
// and would happily apply an unsafe key (e.g. network.p2p.persistent_peers) the
// controller re-asserts on its own SeiNode update plans. A disallowed key
// surfaces as ParamsBuildFailed (not a remote round-trip). Keys are checked in
// sorted order so a multi-key input reports a stable first offender.
//
// This does NOT restart or reload seid; the caller composes RestartSeid /
// StateSync to make the change take effect.
func configPatchParams(cr *seiv1alpha1.SeiNodeTask) (SeiNodeTaskParams, error) {
	p := cr.Spec.ConfigPatch
	if p == nil {
		return SeiNodeTaskParams{}, paramsErr("spec.configPatch is required for kind=ConfigPatch")
	}
	if len(p.Overrides) == 0 {
		return SeiNodeTaskParams{}, paramsErr("config-patch: overrides must not be empty")
	}
	for _, key := range slices.Sorted(maps.Keys(p.Overrides)) {
		if !configPatchKeyAllowed(key) {
			return SeiNodeTaskParams{}, paramsErr(fmt.Sprintf(
				"config-patch: key %q is not in the runtime-safe allowlist (chain.occ_enabled, giga_executor.*, mempool.*)", key))
		}
	}
	return SeiNodeTaskParams{sidecar.TaskTypeConfigApply, &seiconfig.ConfigIntent{
		Incremental: true,
		Overrides:   p.Overrides,
	}}, nil
}

// configPatchKeyAllowed reports whether a dotted sei-config override key is in
// the ConfigPatch runtime-safe allowlist. It is pure allow/deny POLICY on key
// prefixes, not a renderer: sei-config resolves and renders every registered key,
// but the controller admits only families that are safe to patch out-of-band.
// Keep this in sync with the CEL family rule on SeiNodeTaskSpec and
// sdk/sei.configPatchKeyAllowed — it is a value contract across three points.
func configPatchKeyAllowed(key string) bool {
	return key == "chain.occ_enabled" ||
		strings.HasPrefix(key, "giga_executor.") ||
		strings.HasPrefix(key, "mempool.")
}

// resolveSigningUID returns explicit when set; otherwise derives from target
// via ResolveOperatorKeyringUID. With nil target (early-validation path), the
// final marshal happens later from driveTask, which has target.
func resolveSigningUID(explicit string, target *seiv1alpha1.SeiNode) string {
	if explicit != "" {
		return explicit
	}
	if target == nil {
		return ""
	}
	return seiv1alpha1.ResolveOperatorKeyringUID(target)
}
