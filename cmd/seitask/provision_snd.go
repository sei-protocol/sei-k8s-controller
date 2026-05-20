package main

import (
	"context"
	"log"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/sei-protocol/sei-k8s-controller/internal/seitask/provisionsnd"
	"github.com/sei-protocol/sei-k8s-controller/internal/taskruntime"
)

func newProvisionSNDCommand() *cli.Command {
	return &cli.Command{
		Name: "provision-snd",
		Usage: "Apply a SeiNodeDeployment from a YAML spec, wait for Ready + first block, " +
			"and publish role-scoped endpoints to workflow-vars",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:     "role",
				Usage:    "Role tag for workflow-vars keys (e.g. validator, rpc)",
				Sources:  cli.EnvVars("ROLE"),
				Required: true,
			},
			&cli.StringFlag{
				Name:    "name",
				Usage:   "SND metadata.name (defaults to <workflow>-<role>)",
				Sources: cli.EnvVars("SND_NAME"),
			},
			&cli.StringFlag{
				Name:    "spec-file",
				Usage:   "Path to the SeiNodeDeployment YAML",
				Sources: cli.EnvVars("SND_SPEC_FILE"),
				Value:   "/etc/scenario/snd.yaml",
			},
			&cli.StringFlag{
				Name:     "chain-id",
				Usage:    "Chain ID (overrides spec.template.spec.chainId + spec.genesis.chainId)",
				Sources:  cli.EnvVars("CHAIN_ID"),
				Required: true,
			},
			&cli.StringFlag{
				Name:     "image",
				Usage:    "seid container image (overrides spec.template.spec.image)",
				Sources:  cli.EnvVars("SEID_IMAGE"),
				Required: true,
			},
			&cli.IntFlag{
				Name:    "replicas",
				Usage:   "Replica count override (0 = use YAML)",
				Sources: cli.EnvVars("REPLICAS"),
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
	wf, err := taskruntime.LoadWorkflowIdentity()
	if err != nil {
		return err
	}
	c, err := kubeClientFromEnv()
	if err != nil {
		return err
	}

	p := provisionsnd.Params{
		Role:              cmd.String("role"),
		Name:              cmd.String("name"),
		SpecFile:          cmd.String("spec-file"),
		ChainID:           cmd.String("chain-id"),
		Image:             cmd.String("image"),
		Replicas:          int32(cmd.Int("replicas")),
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
	log.Printf("provision-snd: SND %q Ready, chainID=%s, TM=%s", res.Name, res.ChainID, res.Endpoints.TendermintRpc)
	return nil
}
