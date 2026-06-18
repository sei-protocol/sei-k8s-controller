package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/sei-protocol/sei-k8s-controller/internal/seitask/provisionsnd"
	"github.com/sei-protocol/sei-k8s-controller/internal/taskruntime"
)

func newProvisionSNDCommand() *cli.Command {
	return &cli.Command{
		Name: "provision-snd",
		Usage: "Render a SeiNetwork template, apply it, wait for Ready + " +
			"first block, and publish role-scoped endpoints to workflow-vars",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:     "role",
				Usage:    "Role tag for workflow-vars keys (e.g. validator, rpc)",
				Sources:  cli.EnvVars("ROLE"),
				Required: true,
			},
			&cli.StringFlag{
				Name:    "name",
				Usage:   "SeiNetwork metadata.name (defaults to <workflow>-<role>)",
				Sources: cli.EnvVars("SND_NAME"),
			},
			&cli.StringFlag{
				Name:     flagTemplate,
				Usage:    "Path to the Go text/template producing a SeiNetwork YAML",
				Sources:  cli.EnvVars("SND_TEMPLATE"),
				Required: true,
			},
			&cli.StringSliceFlag{
				Name:  flagVar,
				Usage: "KEY=VALUE substitution exposed to the template as .KEY (repeatable)",
			},
			&cli.DurationFlag{
				Name:  "ready-timeout",
				Usage: "Max wait for status.phase=Ready",
				Value: 15 * time.Minute,
			},
			&cli.DurationFlag{
				Name:  "first-block-timeout",
				Usage: "Max post-Ready wait for the chain to produce its first block",
				Value: 5 * time.Minute,
			},
		},
		Action: runProvisionSND,
	}
}

func runProvisionSND(ctx context.Context, cmd *cli.Command) error {
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

	p := provisionsnd.Params{
		Role:              cmd.String("role"),
		Name:              cmd.String("name"),
		TemplatePath:      cmd.String(flagTemplate),
		Vars:              vars,
		ReadyTimeout:      cmd.Duration("ready-timeout"),
		FirstBlockTimeout: cmd.Duration("first-block-timeout"),
		Workflow:          wf,
	}
	res, err := provisionsnd.Run(ctx, c, p)
	if err != nil {
		taskruntime.WriteExitReason(ctx, c, wf, err)
		return err
	}
	taskruntime.WriteExitReason(ctx, c, wf, nil)
	log.Printf("provision-snd: SeiNetwork %q Ready, chainID=%s, TM=%s", res.Name, res.ChainID, res.Endpoints.TendermintRpc)
	return nil
}

func parseKVPairs(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(pairs))
	for _, kv := range pairs {
		idx := strings.IndexByte(kv, '=')
		if idx <= 0 {
			return nil, fmt.Errorf("--var %q must be KEY=VALUE", kv)
		}
		out[kv[:idx]] = kv[idx+1:]
	}
	return out, nil
}
