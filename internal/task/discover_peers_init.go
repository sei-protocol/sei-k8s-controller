package task

import (
	"context"
	"encoding/json"
	"fmt"

	sidecar "github.com/sei-protocol/seictl/sidecar/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// TaskTypeDiscoverPeersInit is the init/bootstrap-path discover-peers task. It
// is distinct from the sidecar-backed sidecar.TaskTypeDiscoverPeers (used by the
// SeiNodeTask DiscoverPeers kind, which fails fast at synthesis on empty
// sources) because its peer sources cannot be frozen at plan-build time: a
// label source resolves to status.resolvedPeers, which reconcilePeers may not
// have populated yet. This task re-derives sources from the live SeiNode on
// every reconcile and never submits a zero-source discover-peers — which would
// wipe persistent-peers.
const TaskTypeDiscoverPeersInit = "discover-peers-init"

// discoverPeersInitExecution re-derives peer sources from the live SeiNode on
// each Execute. While spec.peers resolves to zero sources (e.g. a label source
// whose status.resolvedPeers is not yet populated), Execute returns a transient
// error so the executor retries within the discover-peers retry budget rather
// than submitting an empty write. Once sources resolve, it submits a real
// discover-peers to the sidecar and polls it to completion like any other
// sidecar task.
type discoverPeersInitExecution struct {
	taskBase
	cfg   ExecutionConfig
	inner *sidecarExecution[sidecar.DiscoverPeersTask]
}

func deserializeDiscoverPeersInit(id string, _ json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	return &discoverPeersInitExecution{
		taskBase: taskBase{id: id, status: ExecutionRunning},
		cfg:      cfg,
	}, nil
}

// Execute derives sources from the live node and submits a discover-peers once
// they resolve. Empty sources marks the execution Failed (returning nil, not an
// error): the executor's Status branch then consumes one unit of the
// discover-peers retry budget and re-pends the task, so a label source
// converges once reconcilePeers populates status.resolvedPeers, and an
// unresolvable source fails the plan only after the bounded budget is
// exhausted. Returning an error here instead would loop at the fixed poll
// interval forever, bypassing the retry budget — never bounded.
func (e *discoverPeersInitExecution) Execute(ctx context.Context) error {
	node, err := ResourceAs[*seiv1alpha1.SeiNode](e.cfg)
	if err != nil {
		return Terminal(err)
	}

	sources := DiscoverPeerSources(node)
	if len(sources) == 0 {
		// Never submit a zero-source discover-peers (it would wipe
		// persistent-peers). Defer via the retry budget until sources resolve.
		e.setFailed(fmt.Errorf("discover-peers: no resolved peer sources yet for node %q "+
			"(spec.peers present but unresolved); awaiting status.resolvedPeers", node.Name))
		return nil
	}

	e.inner = &sidecarExecution[sidecar.DiscoverPeersTask]{
		buildSC: e.cfg.BuildSidecarClient,
		id:      e.id,
		params:  sidecar.DiscoverPeersTask{Sources: sources},
		status:  ExecutionRunning,
	}
	return e.inner.Execute(ctx)
}

// Status reports the deferral (Failed, from Execute on empty sources) or
// delegates to the submitted sidecar task. The empty-source Failed status feeds
// the executor's retry-budget branch, which re-pends the task for the next
// reconcile to re-evaluate live sources.
func (e *discoverPeersInitExecution) Status(ctx context.Context) ExecutionStatus {
	if s, done := e.isTerminal(); done {
		return s
	}
	if e.inner == nil {
		return ExecutionRunning
	}
	status := e.inner.Status(ctx)
	if status == ExecutionFailed {
		e.err = e.inner.Err()
	}
	return status
}
