package main

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/sei-protocol/sei-k8s-controller/internal/seitask/provisionnode"
	"github.com/sei-protocol/sei-k8s-controller/internal/taskruntime"
)

// Flag names shared across the template-rendering subcommands (provision-snd,
// provision-node, runner), declared once so goconst stays green.
const (
	flagTemplate = "template"
	flagVar      = "var"
)

func newProvisionNodeCommand() *cli.Command {
	return &cli.Command{
		Name: "provision-node",
		Usage: "Fan out N standalone SeiNode followers from a template, wait for " +
			"Running + per-node TM/EVM readiness, and publish role-scoped endpoints",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:     "role",
				Usage:    "Role tag for workflow-vars keys (e.g. rpc); uppercased to RPC_*",
				Sources:  cli.EnvVars("ROLE"),
				Required: true,
			},
			&cli.StringFlag{
				Name:    "name",
				Usage:   "Base name; the N followers are <name>-0..<name>-(N-1) (defaults to <chainId>-<role>)",
				Sources: cli.EnvVars("NODE_NAME"),
			},
			&cli.StringFlag{
				Name:     flagTemplate,
				Usage:    "Path to the Go text/template producing one kind: SeiNode YAML",
				Sources:  cli.EnvVars("NODE_TEMPLATE"),
				Required: true,
			},
			&cli.StringSliceFlag{
				Name:  flagVar,
				Usage: "KEY=VALUE substitution as .KEY (repeatable); .ORDINAL and .NODE_NAME are runtime-injected",
			},
			&cli.IntFlag{
				Name:    "replicas",
				Usage:   "N: number of follower SeiNode CRs to fan out",
				Sources: cli.EnvVars("NODE_REPLICAS"),
				Value:   1,
			},
			&cli.StringFlag{
				Name:    "network",
				Usage:   "Genesis SeiNetwork to follow; drives peer auto-wiring + the sei.io/seinetwork object label",
				Sources: cli.EnvVars("NETWORK"),
			},
			&cli.StringFlag{
				Name:  "network-namespace",
				Usage: "Namespace of the genesis SeiNetwork for the synthesized peer selector (defaults to the workflow namespace)",
			},
			&cli.DurationFlag{
				Name:  "running-timeout",
				Usage: "Max wait for all N SeiNodes to reach status.phase=Running",
				Value: 15 * time.Minute,
			},
			&cli.DurationFlag{
				Name:  "first-block-timeout",
				Usage: "Per-node post-Running readiness budget (TM /status height>0 and EVM eth_blockNumber 200)",
				Value: 5 * time.Minute,
			},
			&cli.DurationFlag{
				Name:  "poll-interval",
				Usage: "Status + RPC poll cadence",
				Value: 5 * time.Second,
			},
		},
		Action: runProvisionNode,
	}
}

func runProvisionNode(ctx context.Context, cmd *cli.Command) error {
	c, err := kubeClientFromEnv()
	if err != nil {
		return err
	}
	wf, err := taskruntime.LoadWorkflowIdentity(ctx, c)
	if err != nil {
		return err
	}

	vars, err := parseKVPairs(cmd.StringSlice(flagVar))
	if err != nil {
		return err
	}

	p := provisionnode.Params{
		Role:              cmd.String("role"),
		Name:              cmd.String("name"),
		TemplatePath:      cmd.String(flagTemplate),
		Vars:              vars,
		Replicas:          cmd.Int("replicas"),
		Network:           cmd.String("network"),
		NetworkNamespace:  cmd.String("network-namespace"),
		RunningTimeout:    cmd.Duration("running-timeout"),
		FirstBlockTimeout: cmd.Duration("first-block-timeout"),
		PollInterval:      cmd.Duration("poll-interval"),
		Workflow:          wf,
	}
	res, err := provisionnode.Run(ctx, c, p)
	if err != nil {
		taskruntime.WriteExitReason(ctx, c, wf, err)
		return err
	}
	taskruntime.WriteExitReason(ctx, c, wf, nil)
	log.Printf("provision-node: %d SeiNode(s) Running [%s], chainID=%s, EVM_RPC_LIST=%s",
		len(res.Names), strings.Join(res.Names, ","), res.ChainID, res.EVMRPCList)
	return nil
}
