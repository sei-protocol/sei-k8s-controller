// Package loadregression is the nightly load-regression workload, the
// baseline it is gated against, and the gate itself, kept side by side so a
// change to one is reviewed with the others. TestNightlyLoadRegression in
// test/integration runs it.
//
// To change the workload, edit profile.json (sei-load's profile schema) and
// the matching config block in baseline.json; a mismatch fails this package's
// tests. To set the baseline, paste the "load-regression result:" line from
// each recorded nightly into baseline.json's runs; the gate compares against
// their mean.
//
// The thresholds are a few times the spread between two harbor runs of one
// image (included TPS 0.003%, p90 1.6%, CPU 1.2%, memory 1.8%). p50 is the
// exception: it moved 7.1% between those runs, so it keeps a 10% allowance.
// Revisit them once the baseline holds a week of nightlies.
package loadregression

import (
	_ "embed"
	"fmt"
	"strings"

	"github.com/sei-protocol/sei-k8s-controller/harness/bench"
)

//go:embed profile.json
var profileTmpl string

//go:embed baseline.json
var baselineJSON []byte

const (
	// DurationMinutes is sei-load's --duration.
	DurationMinutes = 30
	// WarmupMinutes is dropped from the front of the measured window.
	// sei-load's --duration clock starts before it funds the account pool and
	// deploys the scenario contracts, so the first minutes are setup traffic,
	// not the workload.
	WarmupMinutes = 2
	Validators    = 4
	RPCNodes      = 2
	// RootBalance funds the sei-load root at genesis. The profile disperses
	// 5 SEI to each of 500 accounts (2,500 SEI); the rest covers the root's own
	// deploys and disperse batches with wide margin.
	RootBalance = "1000000000000usei"
)

// RenderProfile fills profile.json for one run. Load is sent to sendEVM only;
// sei-load's inclusion tracker takes its heads and receipts from receiptEVM, a
// follower taking no send load, which is the topology the baseline was
// recorded with and what the tracker needs to be trustworthy.
func RenderProfile(chainID, sendEVM, receiptEVM string) string {
	return strings.ReplaceAll(bench.FillProfile(profileTmpl, chainID, []string{sendEVM}),
		"__RECEIPT_ENDPOINT__", receiptEVM)
}

// RunConfig is this package's run shape, as compared with the baseline's.
func RunConfig() (Config, error) {
	cfg, err := profileShape([]byte(RenderProfile("config", "", "")))
	if err != nil {
		return Config{}, fmt.Errorf("profile.json: %w", err)
	}
	cfg.DurationMinutes = DurationMinutes
	cfg.WarmupMinutes = WarmupMinutes
	cfg.Validators = Validators
	cfg.RPCNodes = RPCNodes
	return cfg, nil
}

// RecordedBaseline parses the embedded baseline.json.
func RecordedBaseline() (Baseline, error) {
	b, err := parseBaseline(baselineJSON)
	if err != nil {
		return Baseline{}, fmt.Errorf("baseline.json: %w", err)
	}
	return b, nil
}
