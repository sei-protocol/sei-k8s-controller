// Command seitask-runner is the orchestration container that Chaos Mesh
// Workflow Task steps use to apply SeiNodeTask CRs and wait for completion.
// See docs/design/seinode-task-lld.md ("Runner container") for the interface
// contract.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/sei-protocol/sei-k8s-controller/internal/runner"
)

// repeatable is a flag.Value that accumulates values on each --flag invocation.
type repeatable []string

func (r *repeatable) String() string     { return strings.Join(*r, ",") }
func (r *repeatable) Set(s string) error { *r = append(*r, s); return nil }

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "seitask-runner:", err)
		os.Exit(1)
	}
}

//nolint:gocyclo // CLI flag wiring is straight-line setup
func run() error {
	var (
		templatePath    string
		vars            repeatable
		outputJSONPaths repeatable
		outputEnvFile   string
		envFile         string
		timeout         time.Duration
		pollInterval    time.Duration
		namespace       string
		perNodeSelector string
		fanoutMode      string
		kubeconfig      string
	)

	flag.StringVar(&templatePath, "template", "",
		"Path to the Go text/template producing a SeiNodeTask manifest (required).")
	flag.Var(&vars, "var",
		"KEY=VALUE substitution exposed to the template as .KEY. Repeatable.")
	flag.Var(&outputJSONPaths, "output-jsonpath",
		"JSONPath=ENV_VAR extraction spec, e.g. '.status.outputs.govVote.txHash=TX_HASH'. Repeatable.")
	flag.StringVar(&outputEnvFile, "output-env-file", "/workflow/vars/env.sh",
		"File to append extracted KEY=value pairs to on Complete.")
	flag.StringVar(&envFile, "env-file", "",
		"Env file to source before render (defaults to /workflow/vars/env.sh when present).")
	flag.DurationVar(&timeout, "timeout", 10*time.Minute,
		"Total poll timeout per SeiNodeTask.")
	flag.DurationVar(&pollInterval, "poll-interval", 5*time.Second,
		"Cadence the runner re-reads status.phase.")
	flag.StringVar(&namespace, "namespace", "",
		"Namespace to apply into (defaults to the SA's namespace).")
	flag.StringVar(&perNodeSelector, "per-node-selector", "",
		"Label selector for fan-out over SeiNodes. Empty = single-node mode.")
	flag.StringVar(&fanoutMode, "fanout-mode", "all-must-succeed",
		"all-must-succeed | best-effort | quorum:N")
	flag.StringVar(&kubeconfig, "kubeconfig", "",
		"Path to kubeconfig (defaults to in-cluster config).")
	flag.Parse()

	if templatePath == "" {
		return fmt.Errorf("--template is required")
	}
	if perNodeSelector != "" {
		if _, ok := vars.toMap()["NODE"]; ok {
			return fmt.Errorf("--per-node-selector is incompatible with --var NODE=...; the runner sets .NODE per match")
		}
	}

	varMap, err := vars.toMapStrict()
	if err != nil {
		return err
	}

	ns, err := resolveNamespace(namespace)
	if err != nil {
		return err
	}

	cfg, err := loadKubeConfig(kubeconfig)
	if err != nil {
		return fmt.Errorf("load kube config: %w", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("create dynamic client: %w", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	r := &runner.Run{
		Opts: runner.Options{
			TemplatePath:    templatePath,
			Vars:            varMap,
			OutputJSONPaths: outputJSONPaths,
			OutputEnvFile:   outputEnvFile,
			EnvFile:         envFile,
			Timeout:         timeout,
			PollInterval:    pollInterval,
			Namespace:       ns,
			PerNodeSelector: perNodeSelector,
			FanoutMode:      fanoutMode,
		},
		Stdout:   os.Stdout,
		Stderr:   os.Stderr,
		Renderer: runner.DefaultRenderer{},
		Applier:  runner.DynamicApplier{Client: dyn},
		Poller:   runner.DynamicPoller{Client: dyn},
		Lister:   runner.DynamicNodeLister{Client: dyn},
		Sourcer:  runner.FileEnvSourcer{},
		Writer:   runner.FileEnvWriter{},
	}
	return r.Execute(ctx)
}

func (r *repeatable) toMap() map[string]string {
	out := map[string]string{}
	for _, v := range *r {
		idx := strings.IndexByte(v, '=')
		if idx <= 0 {
			continue
		}
		out[v[:idx]] = v[idx+1:]
	}
	return out
}

func (r *repeatable) toMapStrict() (map[string]string, error) {
	out := map[string]string{}
	for _, v := range *r {
		idx := strings.IndexByte(v, '=')
		if idx <= 0 {
			return nil, fmt.Errorf("--var %q must be KEY=VALUE", v)
		}
		out[v[:idx]] = v[idx+1:]
	}
	return out, nil
}

// resolveNamespace falls back to the in-pod SA namespace file when the flag
// is empty. Matches kubectl's resolution order.
func resolveNamespace(flagNS string) (string, error) {
	if flagNS != "" {
		return flagNS, nil
	}
	const saNS = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
	b, err := os.ReadFile(saNS)
	if err == nil {
		return strings.TrimSpace(string(b)), nil
	}
	return "", fmt.Errorf("--namespace not provided and SA namespace file unreadable: %w", err)
}

func loadKubeConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}
	// Out-of-cluster fallback for local dev: respect $KUBECONFIG / default path.
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{},
	).ClientConfig()
}
