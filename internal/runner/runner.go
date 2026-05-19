// Package runner implements the seitask-runner orchestration container that
// Chaos Mesh Workflow Task steps use to apply SeiNodeTask CRs and wait for
// completion. The runner is intentionally generic — the per-kind shape comes
// from text/template files mounted at /templates, not from CLI subcommands.
//
// Behavior contract (LLD: docs/design/seinode-task-lld.md "Runner container"):
//
//  1. Source /workflow/vars/env.sh if present (env-file bridge between Workflow
//     steps).
//  2. Render the template with --var KEY=VALUE substitutions. Single-node
//     mode renders once with .NODE set from --var NODE=<name>; fan-out mode
//     lists SeiNodes by selector and renders once per match with .NODE set
//     to each SeiNode's name.
//  3. Server-side apply each rendered SeiNodeTask (fieldOwner=seitask-runner).
//  4. Poll .status.phase until terminal (Complete | Failed) or --timeout.
//  5. On Complete, run --output-jsonpath extractions and append KEY=value
//     lines to --output-env-file. Exit 0.
//  6. On Failed or timeout, exit 1.
//
// The runner talks to the K8s API only (no pods/exec, no kubectl binary).
package runner

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"
)

// Options is the parsed CLI invocation.
type Options struct {
	// TemplatePath is the path to the Go text/template file producing a
	// SeiNodeTask manifest.
	TemplatePath string

	// Vars are KEY=VALUE substitutions exposed to the template as
	// .KEY accessors.
	Vars map[string]string

	// OutputJSONPaths are extraction expressions of the form
	// '.status.outputs.govVote.txHash=TX_HASH'. The left side is a JSONPath
	// against the SeiNodeTask object; the right side is the env var name
	// written to OutputEnvFile.
	OutputJSONPaths []string

	// OutputEnvFile is the path the runner appends KEY=value lines to on
	// Complete. Conventionally /workflow/vars/env.sh.
	OutputEnvFile string

	// Timeout bounds the total poll duration per apply.
	Timeout time.Duration

	// PollInterval is the cadence the runner re-reads .status.phase.
	PollInterval time.Duration

	// Namespace overrides the in-cluster namespace (defaults to the SA
	// namespace mounted at /var/run/secrets/kubernetes.io/serviceaccount/namespace).
	Namespace string

	// PerNodeSelector enables fan-out mode. Empty means single-node.
	// Value is a Kubernetes label selector (e.g. "role=validator").
	PerNodeSelector string

	// FanoutMode selects the success policy: all-must-succeed (default),
	// best-effort, or quorum:N.
	FanoutMode string

	// EnvFile is the env-file the runner sources at startup. When empty,
	// /workflow/vars/env.sh is used if it exists.
	EnvFile string
}

// FanoutPolicy modes.
const (
	fanoutModeAll        = "all"
	fanoutModeBestEffort = "best-effort"
	fanoutModeQuorum     = "quorum"

	// PhaseComplete and PhaseFailed mirror the SeiNodeTask CRD phase values.
	// Centralized here so the runner doesn't import the api types package
	// (keeps the runner build closure small).
	PhaseComplete = "Complete"
	PhaseFailed   = "Failed"
)

// FanoutPolicy is the parsed --fanout-mode value.
type FanoutPolicy struct {
	// Mode is one of "all", "best-effort", "quorum".
	Mode string
	// Quorum is the N from "quorum:N"; zero otherwise.
	Quorum int
}

// ParseFanoutMode parses a --fanout-mode value into a policy. The empty
// string maps to "all-must-succeed" (the default).
//
// Fail-fast semantics: under "all-must-succeed", the runner exits non-zero
// as soon as one target fails — remaining targets are left in-flight, and
// the SeiNodeTaskReconciler keeps reconciling them until they reach a
// terminal phase or are garbage-collected. The runner does NOT delete
// CRs on exit (post-mortem `kubectl describe` would lose context). For
// tx-emitting kinds the broadcast has already happened by the time the
// runner polls, so early-exit changes runner wall-clock but not chain state.
func ParseFanoutMode(s string) (FanoutPolicy, error) {
	switch s {
	case "", "all-must-succeed":
		return FanoutPolicy{Mode: fanoutModeAll}, nil
	case fanoutModeBestEffort:
		return FanoutPolicy{Mode: fanoutModeBestEffort}, nil
	}
	if rest, ok := strings.CutPrefix(s, "quorum:"); ok {
		n, err := parsePositiveInt(rest)
		if err != nil {
			return FanoutPolicy{}, fmt.Errorf("invalid quorum value: %w", err)
		}
		return FanoutPolicy{Mode: fanoutModeQuorum, Quorum: n}, nil
	}
	return FanoutPolicy{}, fmt.Errorf("unknown fanout-mode %q (want all-must-succeed | best-effort | quorum:N)", s)
}

// Decide evaluates a set of per-target outcomes against the policy.
//
//   - all: every outcome must be true; any false fails. Returns ok=true only
//     once every entry is true; ok=false the moment any entry is false.
//   - best-effort: ok=true if at least one outcome is true; ok=false only
//     when every entry has terminated and zero were true.
//   - quorum:N: ok=true once Quorum entries are true; ok=false when so many
//     have failed that Quorum can no longer be reached.
//
// done indicates whether the policy can conclude given the outcomes so far.
// When done=false, the caller should keep polling. When done=true, ok is the
// final verdict.
func (p FanoutPolicy) Decide(outcomes []Outcome, total int) (done, ok bool) {
	var completed, failed, pending int
	for _, o := range outcomes {
		switch o {
		case OutcomeComplete:
			completed++
		case OutcomeFailed:
			failed++
		default:
			pending++
		}
	}
	pending += total - len(outcomes)

	switch p.Mode {
	case fanoutModeAll:
		if failed > 0 {
			return true, false
		}
		if completed == total {
			return true, true
		}
		return false, false
	case fanoutModeBestEffort:
		if completed > 0 && pending == 0 {
			return true, true
		}
		if pending == 0 {
			return true, completed > 0
		}
		return false, false
	case fanoutModeQuorum:
		if completed >= p.Quorum {
			return true, true
		}
		// If remaining successes can no longer reach quorum, fail fast.
		if completed+pending < p.Quorum {
			return true, false
		}
		return false, false
	}
	return true, false
}

// Outcome is the terminal status of one applied SeiNodeTask.
type Outcome int

const (
	// OutcomeUnknown means the task has not reached a terminal phase.
	OutcomeUnknown Outcome = iota
	// OutcomeComplete means .status.phase=Complete.
	OutcomeComplete
	// OutcomeFailed means .status.phase=Failed.
	OutcomeFailed
)

// Run is the top-level entrypoint. It is split out of main so it can be
// unit tested with stubbed K8s and filesystem dependencies.
type Run struct {
	Opts     Options
	Stdout   io.Writer
	Stderr   io.Writer
	Now      func() time.Time
	Renderer Renderer
	Applier  Applier
	Poller   Poller
	Lister   NodeLister
	Sourcer  EnvSourcer
	Writer   EnvWriter
}

// Renderer renders a template file with vars to a SeiNodeTask manifest.
type Renderer interface {
	Render(templatePath string, vars map[string]string) (rendered []byte, name string, err error)
}

// Applier applies a rendered SeiNodeTask manifest to the cluster.
type Applier interface {
	Apply(ctx context.Context, namespace string, manifest []byte) error
}

// Poller polls a SeiNodeTask's status until terminal or context is done.
// Returns the final phase plus the raw object (for jsonpath extraction).
type Poller interface {
	Poll(ctx context.Context, namespace, name string, interval time.Duration) (phase string, obj map[string]any, failureReason string, err error)
}

// NodeLister lists SeiNode names in a namespace matching a label selector.
type NodeLister interface {
	List(ctx context.Context, namespace, selector string) (names []string, err error)
}

// EnvSourcer reads a shell-style env file into a map. Lines that aren't
// KEY=VALUE are skipped silently (commenting/empty/etc).
type EnvSourcer interface {
	Source(path string) (map[string]string, error)
}

// EnvWriter appends KEY=value lines to an env file.
type EnvWriter interface {
	Append(path string, kv []KV) error
}

// KV is a single env-file pair.
type KV struct {
	Key, Value string
}

func parsePositiveInt(s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("empty value")
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("not an integer: %q", s)
		}
		n = n*10 + int(c-'0')
	}
	if n == 0 {
		return 0, fmt.Errorf("must be > 0")
	}
	return n, nil
}
