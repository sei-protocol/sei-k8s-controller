// Package bench renders the seiload Job manifest the nightly benchmark runs
// against a SeiNetwork. It is the single source for that manifest: the
// in-repo suite (test/integration) renders from it, and seictl renders the
// same bytes for engineers to commit under GitOps, so a hand-run benchmark
// and a nightly one load the chain the same way.
//
// The manifest owns seiload's shape (flags, ports, security context,
// resources); only per-run values are templated. The profile itself rides a
// ConfigMap the caller creates, mounted at /etc/seiload/profile.json.
package bench

import (
	"bytes"
	_ "embed"
	"fmt"
	"text/template"
)

//go:embed seiload_job.yaml.tmpl
var jobTmpl string

// DefaultWorkload is the SEILOAD_WORKLOAD value when Params.Workload is unset.
const DefaultWorkload = "nightly"

// Params are the per-run values templated into the seiload Job manifest.
type Params struct {
	// RunID names the Job (seiload-<RunID>) and labels it sei.io/harness-run.
	RunID string
	// ChainID is exported to seiload as SEILOAD_CHAIN_ID.
	ChainID string
	// Commit is the seid commit under test, exported as SEILOAD_COMMIT_ID.
	Commit string
	// Image is the seiload container image reference.
	Image string
	// DurationMinutes is seiload's --duration; the Job's
	// activeDeadlineSeconds is derived from it unless DeadlineSeconds is set.
	DurationMinutes int
	// ProfileCM is the ConfigMap holding profile.json.
	ProfileCM string
	// DeadlineSeconds caps the Job. Zero derives DurationMinutes plus
	// DeadlineSlackMinutes.
	DeadlineSeconds int
	// Namespace is written to metadata when set; the harness leaves it empty
	// and sets it on the decoded object instead.
	Namespace string
	// Workload is exported as SEILOAD_WORKLOAD, a label on the emitted
	// metrics. Empty means DefaultWorkload.
	Workload string
}

// DeadlineSlackMinutes is added to DurationMinutes when deriving the Job's
// activeDeadlineSeconds: image pull plus the post-summary metrics flush.
const DeadlineSlackMinutes = 15

// Render templates the Job manifest. It rejects empty RunID, ChainID, Image
// and ProfileCM, a non-positive DurationMinutes and a negative
// DeadlineSeconds.
func Render(p Params) ([]byte, error) {
	switch {
	case p.RunID == "":
		return nil, fmt.Errorf("seiload job: runID is required")
	case p.ChainID == "":
		return nil, fmt.Errorf("seiload job: chainID is required")
	case p.Image == "":
		return nil, fmt.Errorf("seiload job: image is required")
	case p.ProfileCM == "":
		return nil, fmt.Errorf("seiload job: profileCM is required")
	case p.DurationMinutes <= 0:
		return nil, fmt.Errorf("seiload job: durationMinutes must be positive, got %d", p.DurationMinutes)
	case p.DeadlineSeconds < 0:
		return nil, fmt.Errorf("seiload job: deadlineSeconds must not be negative, got %d", p.DeadlineSeconds)
	}
	if p.DeadlineSeconds == 0 {
		p.DeadlineSeconds = (p.DurationMinutes + DeadlineSlackMinutes) * 60
	}
	if p.Workload == "" {
		p.Workload = DefaultWorkload
	}
	tmpl, err := template.New("seiload-job").Parse(jobTmpl)
	if err != nil {
		return nil, fmt.Errorf("seiload job: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, p); err != nil {
		return nil, fmt.Errorf("seiload job: %w", err)
	}
	return buf.Bytes(), nil
}
