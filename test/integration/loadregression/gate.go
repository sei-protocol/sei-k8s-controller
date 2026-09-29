package loadregression

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"reflect"
	"slices"
	"time"
)

// Metric names measured from a load window. They key both Metrics and a
// baseline's thresholds, so a new metric is one entry in Measure plus one
// threshold in baseline.json.
const (
	MetricIncludedTPS        = "includedTps"
	MetricBlockIntervalP50Ms = "blockIntervalP50Ms"
	MetricBlockIntervalP90Ms = "blockIntervalP90Ms"
)

// Block is one committed block as the chain reports it.
type Block struct {
	Height int64
	Time   time.Time
	NumTxs int64
}

// Metrics are named values measured from one run.
type Metrics map[string]float64

// Measurement is a window's metrics plus what they were computed over.
type Measurement struct {
	Metrics Metrics
	Blocks  int
	Txs     int64
	Window  time.Duration
}

// Measure computes the load metrics over the blocks timestamped in [from, to].
//
// Throughput is what reached a block divided by the window, not what the
// generator offered: at a fixed offered rate the generator's own TPS reads
// its setting until the chain saturates. Block intervals come from the
// chain's block timestamps, not the generator's polling of them.
//
// blocks must extend to or past both edges, so a window the caller did not
// fully fetch is an error rather than a quietly short sample.
func Measure(blocks []Block, from, to time.Time) (Measurement, error) {
	if !to.After(from) {
		return Measurement{}, fmt.Errorf("window %s..%s is empty", from.Format(time.RFC3339), to.Format(time.RFC3339))
	}
	sorted := slices.Clone(blocks)
	slices.SortFunc(sorted, func(a, b Block) int { return cmp.Compare(a.Height, b.Height) })
	if len(sorted) == 0 || sorted[0].Time.After(from) || sorted[len(sorted)-1].Time.Before(to) {
		return Measurement{}, fmt.Errorf("blocks do not cover the window %s..%s",
			from.Format(time.RFC3339), to.Format(time.RFC3339))
	}

	var in []Block
	var txs int64
	for _, b := range sorted {
		if !b.Time.Before(from) && !b.Time.After(to) {
			in = append(in, b)
			txs += b.NumTxs
		}
	}
	if len(in) < 2 {
		return Measurement{}, fmt.Errorf("%d blocks in the window, need at least 2", len(in))
	}

	intervals := make([]float64, 0, len(in)-1)
	for i := 1; i < len(in); i++ {
		intervals = append(intervals, float64(in[i].Time.Sub(in[i-1].Time).Microseconds())/1000)
	}
	slices.Sort(intervals)

	window := to.Sub(from)
	return Measurement{
		Metrics: Metrics{
			MetricIncludedTPS:        float64(txs) / window.Seconds(),
			MetricBlockIntervalP50Ms: percentile(intervals, 50),
			MetricBlockIntervalP90Ms: percentile(intervals, 90),
		},
		Blocks: len(in),
		Txs:    txs,
		Window: window,
	}, nil
}

// percentile is nearest-rank over sorted values, the definition the recorded
// noise figures for these metrics were computed with.
func percentile(sorted []float64, p float64) float64 {
	i := int(math.Ceil(p/100*float64(len(sorted)))) - 1
	return sorted[min(len(sorted)-1, max(0, i))]
}

// Config is everything that shapes the numbers. A baseline is only comparable
// with a run of the same shape.
type Config struct {
	Scenarios       map[string]int `json:"scenarios"`
	TPS             float64        `json:"tps"`
	Accounts        int            `json:"accounts"`
	DurationMinutes int            `json:"durationMinutes"`
	WarmupMinutes   int            `json:"warmupMinutes"`
	Validators      int            `json:"validators"`
	RPCNodes        int            `json:"rpcNodes"`
}

// profileShape reads the scenario weights, offered TPS and account count from
// a sei-load profile.
func profileShape(profile []byte) (Config, error) {
	var p struct {
		Accounts struct {
			Count int `json:"count"`
		} `json:"accounts"`
		Scenarios []struct {
			Name   string `json:"name"`
			Weight int    `json:"weight"`
		} `json:"scenarios"`
		Settings struct {
			TPS float64 `json:"tps"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(profile, &p); err != nil {
		return Config{}, err
	}
	cfg := Config{Scenarios: map[string]int{}, TPS: p.Settings.TPS, Accounts: p.Accounts.Count}
	for _, s := range p.Scenarios {
		// sei-load treats an omitted weight as 1.
		cfg.Scenarios[s.Name] += max(s.Weight, 1)
	}
	return cfg, nil
}

// direction is which way a metric moving counts as a regression.
type direction string

const (
	up   direction = "up"
	down direction = "down"
)

// Threshold bounds one metric against its baseline mean. The allowance is the
// larger of TolerancePct of the mean and ToleranceAbs in the metric's units,
// so a near-zero mean does not turn any movement into a regression. One-sided:
// the chain getting faster never fails the gate.
type Threshold struct {
	Direction    direction `json:"direction"`
	TolerancePct float64   `json:"tolerancePct"`
	ToleranceAbs float64   `json:"toleranceAbs"`
}

// BaselineRun is one recorded run. The baseline is the mean over all of them;
// the provenance fields are for whoever reads the file.
type BaselineRun struct {
	Metrics    Metrics `json:"metrics"`
	SeidImage  string  `json:"seidImage,omitempty"`
	RecordedAt string  `json:"recordedAt,omitempty"`
}

// Baseline is the reference a run is gated against.
type Baseline struct {
	Config     Config               `json:"config"`
	Runs       []BaselineRun        `json:"runs"`
	Thresholds map[string]Threshold `json:"thresholds"`
}

// parseBaseline decodes and validates a baseline file. Unknown keys are
// rejected so a misspelled threshold cannot silently stop being enforced.
func parseBaseline(data []byte) (Baseline, error) {
	var b Baseline
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		return Baseline{}, err
	}
	for name, th := range b.Thresholds {
		if th.Direction != up && th.Direction != down {
			return Baseline{}, fmt.Errorf("threshold %s: direction %q, want %q or %q", name, th.Direction, up, down)
		}
		if th.TolerancePct < 0 || th.ToleranceAbs < 0 {
			return Baseline{}, fmt.Errorf("threshold %s: tolerances must not be negative", name)
		}
		for i, r := range b.Runs {
			if _, ok := r.Metrics[name]; !ok {
				return Baseline{}, fmt.Errorf("run %d has no %s, which a threshold gates on", i, name)
			}
		}
	}
	if len(b.Runs) == 0 {
		return Baseline{}, fmt.Errorf("no runs; paste at least one load-regression result line")
	}
	return b, nil
}

// mean is the per-metric mean over the recorded runs.
func (b Baseline) mean() Metrics {
	out := Metrics{}
	for name := range b.Thresholds {
		var sum float64
		for _, r := range b.Runs {
			sum += r.Metrics[name]
		}
		out[name] = sum / float64(len(b.Runs))
	}
	return out
}

// Comparable reports why a run of shape cfg cannot be gated against b, or nil.
func (b Baseline) Comparable(cfg Config) error {
	if !reflect.DeepEqual(b.Config, cfg) {
		return fmt.Errorf("run config %+v differs from the baseline's %+v; re-record the baseline for the new shape",
			cfg, b.Config)
	}
	return nil
}

// Verdict is the gate's outcome. Unevaluable and Regressions are kept apart
// because "main got slower" and "this run cannot be compared" need different
// responses from whoever reads the nightly.
type Verdict struct {
	Unevaluable []string
	Regressions []string
	// Lines is one human-readable comparison per gated metric.
	Lines []string
}

// Check gates a run's metrics against the baseline mean. The caller has
// already established the run is Comparable.
func Check(b Baseline, m Metrics) Verdict {
	var v Verdict
	mean := b.mean()
	for _, name := range slices.Sorted(maps.Keys(b.Thresholds)) {
		th := b.Thresholds[name]
		cur, ok := m[name]
		if !ok {
			v.Unevaluable = append(v.Unevaluable, name+" was not measured")
			continue
		}
		base := mean[name]
		allowance := max(math.Abs(base)*th.TolerancePct/100, th.ToleranceAbs)
		worse := cur - base
		if th.Direction == down {
			worse = -worse
		}
		line := fmt.Sprintf("%s: %.2f vs baseline %.2f (%+.1f%%), regression if %s by more than %.2f",
			name, cur, base, (cur-base)/base*100, th.Direction, allowance)
		if worse > allowance {
			v.Regressions = append(v.Regressions, line)
			line += " → REGRESSION"
		} else {
			line += " → ok"
		}
		v.Lines = append(v.Lines, line)
	}
	return v
}
