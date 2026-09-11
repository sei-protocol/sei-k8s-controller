// Package faults is the catalog of Chaos-Mesh fault templates the nightly
// chaos suite injects against a SeiNetwork. It is the single source for
// those manifests: the in-repo suite (test/integration) renders from it, and
// seictl renders the same bytes for engineers to commit under GitOps, so an
// experiment run by hand and one run by the nightly suite exercise the same
// fault.
//
// Every fault is bounded to f=1 on a four-validator network: it targets
// validator 0 (sei.io/node=<chainID>-0) or exactly one pod (mode: one), so a
// 2/3 quorum holds and the chain must stay live under it. Duration-bearing
// faults self-expire through spec.duration; one-shot kills (pod-failure,
// container-kill) carry no duration and end when the kill lands.
package faults

import (
	"bytes"
	"embed"
	"fmt"
	"slices"
	"strings"
	"text/template"
	"time"
)

//go:embed *.yaml.tmpl
var templates embed.FS

// Params are the per-run values templated into a fault manifest.
type Params struct {
	// ChainID selects the target pool through the sei.io/nodedeployment
	// label; the validator-0 selectors derive sei.io/node=<ChainID>-0 from it.
	ChainID string
	// RunID names the CR (<fault>-<RunID>) and labels it sei.io/harness-run.
	RunID string
	// Namespace is the SeiNetwork's namespace; the selector is pinned to it.
	Namespace string
	// Duration is spec.duration for duration-bearing faults, in Go duration
	// syntax. Ignored by one-shot faults.
	Duration string
}

// Fault describes one catalog entry.
type Fault struct {
	// Name is the catalog key, e.g. "network-partition".
	Name string
	// Kind is the Chaos-Mesh CR kind (NetworkChaos, StressChaos, ...).
	Kind string
	// OneShot marks a kill fault: no spec.duration and no AllRecovered
	// condition, so callers skip the self-expiry check and the recovery gate.
	OneShot bool
	// Summary is one line on what the fault does and to whom.
	Summary string

	file string
}

// Catalog is every fault in the package, in the order the nightly suite runs
// them. Deferred faults (dns-chaos, disk-io-latency) are not here; see
// test/integration/chaos_deferred_test.go for why.
const (
	kindNetworkChaos = "NetworkChaos"
	kindStressChaos  = "StressChaos"
	kindTimeChaos    = "TimeChaos"
	kindPodChaos     = "PodChaos"
)

var Catalog = []Fault{
	{Name: "network-partition", Kind: kindNetworkChaos,
		Summary: "isolate validator-0 from validators 1-3 in both directions", file: "network_partition.yaml.tmpl"},
	{Name: "packet-loss", Kind: kindNetworkChaos,
		Summary: "drop a share of packets between one validator and the rest", file: "packet_loss.yaml.tmpl"},
	{Name: "cpu-stress", Kind: kindStressChaos,
		Summary: "burn CPU inside one validator pod", file: "cpu_stress.yaml.tmpl"},
	{Name: "time-skew", Kind: kindTimeChaos,
		Summary: "skew the clock of one validator", file: "time_skew.yaml.tmpl"},
	{Name: "network-latency", Kind: kindNetworkChaos,
		Summary: "add latency on the validator mesh", file: "network_latency.yaml.tmpl"},
	{Name: "bandwidth-limit", Kind: kindNetworkChaos,
		Summary: "cap bandwidth between one validator and the rest", file: "bandwidth_limit.yaml.tmpl"},
	{Name: "memory-stress", Kind: kindStressChaos,
		Summary: "pressure memory inside one validator pod", file: "memory_stress.yaml.tmpl"},
	{Name: "byzantine", Kind: kindNetworkChaos,
		Summary: "corrupt packets from one validator to the rest", file: "byzantine.yaml.tmpl"},
	{Name: "pod-failure", Kind: kindPodChaos, OneShot: true,
		Summary: "kill exactly one validator pod once", file: "pod_failure.yaml.tmpl"},
	{Name: "container-kill", Kind: kindPodChaos, OneShot: true,
		Summary: "kill the seid container of exactly one validator once", file: "container_kill.yaml.tmpl"},
}

// Resource is the CR's plural resource name for dynamic-client GVR lookups.
// Chaos-Mesh pluralises every fault kind by lowercasing it.
func (f Fault) Resource() string { return strings.ToLower(f.Kind) }

// Names lists the catalog keys in catalog order.
func Names() []string {
	names := make([]string, 0, len(Catalog))
	for _, f := range Catalog {
		names = append(names, f.Name)
	}
	return names
}

// Lookup returns the catalog entry for name.
func Lookup(name string) (Fault, error) {
	i := slices.IndexFunc(Catalog, func(f Fault) bool { return f.Name == name })
	if i < 0 {
		return Fault{}, fmt.Errorf("unknown fault %q; known: %v", name, Names())
	}
	return Catalog[i], nil
}

// Render templates the fault's manifest. It rejects an empty ChainID, RunID
// or Namespace, and for duration-bearing faults an unparsable or non-positive
// Duration, so a manifest never reaches the apiserver with an empty selector.
func (f Fault) Render(p Params) ([]byte, error) {
	if err := f.validate(p); err != nil {
		return nil, fmt.Errorf("fault %s: %w", f.Name, err)
	}
	src, err := templates.ReadFile(f.file)
	if err != nil {
		return nil, fmt.Errorf("fault %s: %w", f.Name, err)
	}
	tmpl, err := template.New(f.Name).Option("missingkey=error").Parse(string(src))
	if err != nil {
		return nil, fmt.Errorf("fault %s: %w", f.Name, err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, p); err != nil {
		return nil, fmt.Errorf("fault %s: %w", f.Name, err)
	}
	return buf.Bytes(), nil
}

func (f Fault) validate(p Params) error {
	switch {
	case p.ChainID == "":
		return fmt.Errorf("chainID is required")
	case p.RunID == "":
		return fmt.Errorf("runID is required")
	case p.Namespace == "":
		return fmt.Errorf("namespace is required")
	}
	if f.OneShot {
		return nil
	}
	d, err := time.ParseDuration(p.Duration)
	if err != nil {
		return fmt.Errorf("duration %q: %w", p.Duration, err)
	}
	if d <= 0 {
		return fmt.Errorf("duration must be positive, got %s", d)
	}
	return nil
}
