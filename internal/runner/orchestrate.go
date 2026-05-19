package runner

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Execute runs the full single-or-fanout orchestration for r and returns
// nil on success or an error suitable for printing to stderr + exit 1.
//
//nolint:gocyclo // single linear orchestration; splitting hurts readability
func (r *Run) Execute(ctx context.Context) error {
	if r.Now == nil {
		r.Now = time.Now
	}

	if err := r.sourceEnv(); err != nil {
		return fmt.Errorf("source env file: %w", err)
	}

	policy, err := ParseFanoutMode(r.Opts.FanoutMode)
	if err != nil {
		return err
	}

	targets, err := r.buildTargets(ctx)
	if err != nil {
		return err
	}

	// Apply all targets up-front so they reconcile in parallel.
	for _, t := range targets {
		if err := r.Applier.Apply(ctx, r.Opts.Namespace, t.Manifest); err != nil {
			return fmt.Errorf("apply %s (node=%s): %w", t.Name, t.Node, err)
		}
		_, _ = fmt.Fprintf(r.Stdout, "applied SeiNodeTask %s (node=%s)\n", t.Name, t.Node)
	}

	// Single-node short-circuit avoids the goroutine-per-target overhead and
	// preserves the natural exit-code semantics (failureReason on the single
	// task is the runner's exit error).
	if len(targets) == 1 {
		return r.pollSingle(ctx, targets[0])
	}
	return r.pollFanout(ctx, targets, policy)
}

func (r *Run) sourceEnv() error {
	path := r.Opts.EnvFile
	if path == "" {
		path = "/workflow/vars/env.sh"
	}
	pairs, err := r.Sourcer.Source(path)
	if err != nil {
		return err
	}
	// Sourced values are merged into Vars only if not already set on the CLI;
	// CLI --var takes precedence so an explicit override at the Workflow step
	// level wins over a stale env-file value.
	if r.Opts.Vars == nil {
		r.Opts.Vars = map[string]string{}
	}
	for k, v := range pairs {
		if _, set := r.Opts.Vars[k]; !set {
			r.Opts.Vars[k] = v
		}
	}
	return nil
}

func (r *Run) buildTargets(ctx context.Context) ([]FanoutTarget, error) {
	// Single-node mode: render once with whatever NODE the operator passed via --var.
	if r.Opts.PerNodeSelector == "" {
		manifest, name, err := r.Renderer.Render(r.Opts.TemplatePath, r.Opts.Vars)
		if err != nil {
			return nil, fmt.Errorf("render: %w", err)
		}
		node := r.Opts.Vars["NODE"]
		return []FanoutTarget{{Node: node, Name: name, Manifest: manifest}}, nil
	}
	// Fan-out: discover SeiNodes by selector, render one CR per match.
	nodes, err := r.Lister.List(ctx, r.Opts.Namespace, r.Opts.PerNodeSelector)
	if err != nil {
		return nil, err
	}
	return RenderFanout(r.Renderer, r.Opts.TemplatePath, r.Opts.Vars, nodes)
}

func (r *Run) pollSingle(ctx context.Context, t FanoutTarget) error {
	pollCtx, cancel := context.WithTimeout(ctx, r.Opts.Timeout)
	defer cancel()
	phase, obj, reason, err := r.Poller.Poll(pollCtx, r.Opts.Namespace, t.Name, r.Opts.PollInterval)
	if err != nil {
		return err
	}
	if phase == PhaseFailed {
		return fmt.Errorf("SeiNodeTask %s failed: %s", t.Name, reason)
	}
	return r.writeOutputs(obj)
}

func (r *Run) pollFanout(ctx context.Context, targets []FanoutTarget, policy FanoutPolicy) error {
	pollCtx, cancel := context.WithTimeout(ctx, r.Opts.Timeout)
	defer cancel()

	type result struct {
		idx     int
		outcome Outcome
		obj     map[string]any
		reason  string
		err     error
	}

	// Goroutine-per-target: each loops on Poll until its task is terminal or
	// the shared deadline cancels. We aggregate decisions as results arrive.
	resultCh := make(chan result, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func(idx int, target FanoutTarget) {
			defer wg.Done()
			phase, obj, reason, err := r.Poller.Poll(pollCtx, r.Opts.Namespace, target.Name, r.Opts.PollInterval)
			out := OutcomeUnknown
			switch {
			case err != nil:
				// Treat poll error (incl. deadline) as failed outcome with the underlying error message.
				out = OutcomeFailed
				if reason == "" {
					reason = err.Error()
				}
			case phase == PhaseComplete:
				out = OutcomeComplete
			case phase == PhaseFailed:
				out = OutcomeFailed
			}
			resultCh <- result{idx: idx, outcome: out, obj: obj, reason: reason, err: err}
		}(i, t)
	}
	go func() { wg.Wait(); close(resultCh) }()

	outcomes := make([]Outcome, 0, len(targets))
	objects := make([]map[string]any, len(targets))
	var firstFailure string

	for res := range resultCh {
		outcomes = append(outcomes, res.outcome)
		if res.obj != nil {
			objects[res.idx] = res.obj
		}
		if res.outcome == OutcomeFailed && firstFailure == "" {
			firstFailure = fmt.Sprintf("%s: %s", targets[res.idx].Name, res.reason)
		}
		_, _ = fmt.Fprintf(r.Stdout, "fanout %s (node=%s): %s\n", targets[res.idx].Name, targets[res.idx].Node, outcomeLabel(res.outcome))

		done, ok := policy.Decide(outcomes, len(targets))
		if done {
			// Early-exit: cancel in-flight pollers; they'll surface as failed/unknown
			// and be discarded (we already have a verdict).
			cancel()
			//nolint:revive // intentionally drain the channel to let the WaitGroup complete cleanly
			for range resultCh {
			}
			if !ok {
				if firstFailure == "" {
					firstFailure = "fanout policy not satisfied"
				}
				return errors.New(firstFailure)
			}
			return r.writeFanoutOutputs(objects)
		}
	}

	// Channel drained without an early verdict (all outcomes in but Decide
	// kept saying "not done"). That is a logic bug — Decide must terminate
	// once total entries are seen.
	done, ok := policy.Decide(outcomes, len(targets))
	if !done {
		return fmt.Errorf("fanout policy %q failed to terminate with %d/%d outcomes (internal bug)", r.Opts.FanoutMode, len(outcomes), len(targets))
	}
	if !ok {
		if firstFailure == "" {
			firstFailure = "fanout policy not satisfied"
		}
		return errors.New(firstFailure)
	}
	return r.writeFanoutOutputs(objects)
}

func outcomeLabel(o Outcome) string {
	switch o {
	case OutcomeComplete:
		return "Complete"
	case OutcomeFailed:
		return "Failed"
	default:
		return "Pending"
	}
}

func (r *Run) writeOutputs(obj map[string]any) error {
	if obj == nil || len(r.Opts.OutputJSONPaths) == 0 {
		return nil
	}
	kvs, err := ExtractOutputs(r.Opts.OutputJSONPaths, obj)
	if err != nil {
		return fmt.Errorf("extract outputs: %w", err)
	}
	if r.Opts.OutputEnvFile == "" {
		// Nowhere to write — emit to stdout as a fallback so the runner is
		// still useful when invoked standalone.
		for _, kv := range kvs {
			_, _ = fmt.Fprintf(r.Stdout, "%s=%s\n", kv.Key, kv.Value)
		}
		return nil
	}
	return r.Writer.Append(r.Opts.OutputEnvFile, kvs)
}

// writeFanoutOutputs writes outputs from the *first* Complete object only.
// Fan-out jsonpath extraction across N nodes is undefined in the LLD — the
// canonical use case (UpdateNodeImage fanout) yields the same appliedImage
// per node, so first-Complete is a reasonable convention.
//
// If operators need per-node aggregation later, the right primitive is a
// separate --output-jsonpath-fanout flag with an explicit aggregation policy
// (join, json-array, max). Punted until a scenario demands it.
func (r *Run) writeFanoutOutputs(objects []map[string]any) error {
	for _, obj := range objects {
		if obj == nil {
			continue
		}
		if nestedString(obj, "status", "phase") == PhaseComplete {
			return r.writeOutputs(obj)
		}
	}
	return nil
}

// nestedString is a small map walker that returns the string value at the
// given key path, or "" if any intermediate node is missing or non-string.
// Kept inline so this file doesn't pull apimachinery just for one helper.
func nestedString(obj map[string]any, fields ...string) string {
	var cur any = obj
	for _, f := range fields {
		m, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = m[f]
	}
	s, _ := cur.(string)
	return s
}
