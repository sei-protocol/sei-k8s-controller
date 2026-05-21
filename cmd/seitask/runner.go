package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/urfave/cli/v3"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/sei-protocol/sei-k8s-controller/internal/runner"
	"github.com/sei-protocol/sei-k8s-controller/internal/taskruntime"
)

// newRunnerCommand wires the legacy seitask-runner CLI as a subcommand of
// the monolithic seitask binary. Flag names and semantics match the old
// standalone binary so scenario YAMLs only need to prepend "runner" to args.
// Implementation delegates to internal/runner unchanged.
func newRunnerCommand() *cli.Command {
	return &cli.Command{
		Name:  "runner",
		Usage: "Apply a SeiNodeTask CR from a template and poll until terminal",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:     "template",
				Usage:    "Path to the Go text/template producing a SeiNodeTask manifest (required)",
				Required: true,
			},
			&cli.StringSliceFlag{
				Name:  "var",
				Usage: "KEY=VALUE substitution exposed to the template as .KEY (repeatable)",
			},
			&cli.StringSliceFlag{
				Name:  "output-jsonpath",
				Usage: "JSONPath=ENV_VAR extraction (repeatable)",
			},
			&cli.StringFlag{
				Name:  "output-env-file",
				Usage: "File to append extracted KEY=value pairs to on Complete",
				Value: "/workflow/vars/env.sh",
			},
			&cli.StringFlag{
				Name:  "env-file",
				Usage: "Env file to source before render (defaults to /workflow/vars/env.sh when present)",
			},
			&cli.DurationFlag{
				Name:  "timeout",
				Usage: "Total poll timeout per SeiNodeTask",
				Value: 10 * time.Minute,
			},
			&cli.DurationFlag{
				Name:  "poll-interval",
				Usage: "Cadence the runner re-reads status.phase",
				Value: 5 * time.Second,
			},
			&cli.StringFlag{
				Name:  "namespace",
				Usage: "Namespace to apply into (defaults to the SA's namespace)",
			},
			&cli.StringFlag{
				Name:  "per-node-selector",
				Usage: "Label selector for fan-out over SeiNodes. Empty = single-node mode",
			},
			&cli.StringFlag{
				Name:  "fanout-mode",
				Usage: "all-must-succeed | best-effort | quorum:N",
				Value: "all-must-succeed",
			},
			&cli.StringFlag{
				Name:  "kubeconfig",
				Usage: "Path to kubeconfig (defaults to in-cluster config)",
			},
		},
		Action: runRunner,
	}
}

func runRunner(ctx context.Context, cmd *cli.Command) error {
	varMap, err := parseKVSlice(cmd.StringSlice("var"))
	if err != nil {
		return err
	}
	if cmd.String("per-node-selector") != "" {
		if _, ok := varMap["NODE"]; ok {
			return fmt.Errorf("--per-node-selector is incompatible with --var NODE=...; the runner sets .NODE per match")
		}
	}
	ns, err := resolveRunnerNamespace(cmd.String("namespace"))
	if err != nil {
		return err
	}
	cfg, err := loadRunnerKubeConfig(cmd.String("kubeconfig"))
	if err != nil {
		return fmt.Errorf("load kube config: %w", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("create dynamic client: %w", err)
	}

	// Load the parent Workflow's identity so applied SeiNodeTask CRs
	// carry an ownerRef to it — deleting the Workflow then cascades the
	// per-step SeiNodeTasks. Matches the keygen / provision-snd pattern.
	cliClient, err := kubeClientFromEnv()
	if err != nil {
		return err
	}
	wf, err := taskruntime.LoadWorkflowIdentity(ctx, cliClient)
	if err != nil {
		return err
	}
	ownerRef := wf.OwnerRef()

	r := &runner.Run{
		Opts: runner.Options{
			TemplatePath:    cmd.String("template"),
			Vars:            varMap,
			OutputJSONPaths: cmd.StringSlice("output-jsonpath"),
			OutputEnvFile:   cmd.String("output-env-file"),
			EnvFile:         cmd.String("env-file"),
			Timeout:         cmd.Duration("timeout"),
			PollInterval:    cmd.Duration("poll-interval"),
			Namespace:       ns,
			PerNodeSelector: cmd.String("per-node-selector"),
			FanoutMode:      cmd.String("fanout-mode"),
		},
		Stdout:   os.Stdout,
		Stderr:   os.Stderr,
		Renderer: runner.DefaultRenderer{OwnerRef: &ownerRef},
		Applier:  runner.DynamicApplier{Client: dyn},
		Poller:   runner.DynamicPoller{Client: dyn},
		Lister:   runner.DynamicNodeLister{Client: dyn},
		Sourcer:  runner.FileEnvSourcer{},
		Writer:   runner.FileEnvWriter{},
	}
	return r.Execute(ctx)
}

func parseKVSlice(in []string) (map[string]string, error) {
	out := map[string]string{}
	for _, v := range in {
		idx := strings.IndexByte(v, '=')
		if idx <= 0 {
			return nil, fmt.Errorf("--var %q must be KEY=VALUE", v)
		}
		out[v[:idx]] = v[idx+1:]
	}
	return out, nil
}

// resolveRunnerNamespace falls back to the in-pod SA namespace file when
// --namespace is empty, matching kubectl's resolution order.
func resolveRunnerNamespace(flagNS string) (string, error) {
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

func loadRunnerKubeConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{},
	).ClientConfig()
}
