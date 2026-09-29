package loadregression

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	promapi "github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
)

// Resource metrics, read from Prometheus' cAdvisor series for the validators'
// seid container with the definitions arctic-tests' version-benchmark
// calibrated its noise figures on: per pod the mean CPU rate and the peak
// working set over the window, each then averaged across the validators.
// Averaging matters because identical validators in one run differ by up to
// 14% in CPU, so gating any single pod would gate that pod's luck.
const (
	MetricCPUMeanCores        = "cpuMeanCores"
	MetricMemWorkingSetMaxMiB = "memWorkingSetMaxMiB"
)

// ResourceStep is the resolution resources are sampled at over the window.
const ResourceStep = 15 * time.Second

const resourceMinCoverage = 0.9

// ValidatorResources reads the validators' CPU and memory over [from, to] from
// the Prometheus at promURL. Every pod must have a series covering at least 90%
// of the window, so a scrape gap is an error rather than a quietly short sample.
func ValidatorResources(
	ctx context.Context, promURL, namespace string, pods []string, from, to time.Time,
) (Metrics, error) {
	if len(pods) == 0 {
		return nil, fmt.Errorf("no validator pods")
	}
	client, err := promapi.NewClient(promapi.Config{Address: promURL})
	if err != nil {
		return nil, err
	}
	api := promv1.NewAPI(client)
	quoted := make([]string, len(pods))
	for i, p := range pods {
		quoted[i] = regexp.QuoteMeta(p)
	}
	sel := fmt.Sprintf(`{namespace=%q,pod=~%q,container="seid"}`, namespace, strings.Join(quoted, "|"))

	// max by (pod) collapses duplicate scrapes of one container into one series.
	cpu, err := queryRange(ctx, api,
		fmt.Sprintf("max by (pod) (rate(container_cpu_usage_seconds_total%s[1m]))", sel), from, to)
	if err != nil {
		return nil, fmt.Errorf("cpu: %w", err)
	}
	mem, err := queryRange(ctx, api, fmt.Sprintf("max by (pod) (container_memory_working_set_bytes%s)", sel), from, to)
	if err != nil {
		return nil, fmt.Errorf("memory: %w", err)
	}

	want := int(to.Sub(from)/ResourceStep) + 1
	var cpuSum, memSum float64
	for _, pod := range pods {
		c, m := cpu[pod], mem[pod]
		if float64(min(len(c), len(m))) < resourceMinCoverage*float64(want) {
			return nil, fmt.Errorf("pod %s has %d cpu and %d memory samples, want at least %.0f%% of %d",
				pod, len(c), len(m), resourceMinCoverage*100, want)
		}
		var sum, peak float64
		for _, v := range c {
			sum += v
		}
		for _, v := range m {
			peak = max(peak, v)
		}
		cpuSum += sum / float64(len(c))
		memSum += peak
	}
	n := float64(len(pods))
	return Metrics{
		MetricCPUMeanCores:        cpuSum / n,
		MetricMemWorkingSetMaxMiB: memSum / n / (1 << 20),
	}, nil
}

// queryRange runs a range query at ResourceStep and returns each series'
// values keyed by its pod label.
func queryRange(ctx context.Context, api promv1.API, query string, from, to time.Time) (map[string][]float64, error) {
	v, _, err := api.QueryRange(ctx, query, promv1.Range{Start: from, End: to, Step: ResourceStep})
	if err != nil {
		return nil, err
	}
	matrix, ok := v.(model.Matrix)
	if !ok {
		return nil, fmt.Errorf("got %s, want a matrix", v.Type())
	}
	out := map[string][]float64{}
	for _, s := range matrix {
		pod := string(s.Metric["pod"])
		for _, p := range s.Values {
			out[pod] = append(out[pod], float64(p.Value))
		}
	}
	return out, nil
}
