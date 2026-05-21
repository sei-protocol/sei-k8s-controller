package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/urfave/cli/v3"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/seitask/provisionsnd"
	"github.com/sei-protocol/sei-k8s-controller/internal/taskruntime"
)

func newProvisionSNDCommand() *cli.Command {
	return &cli.Command{
		Name: "provision-snd",
		Usage: "Apply a SeiNodeDeployment from a bundled preset with --override / " +
			"--genesis-account deltas, wait for Ready + first block, and publish " +
			"role-scoped endpoints to workflow-vars",
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
				Name:     "preset",
				Usage:    "Bundled SND preset (validator | full-node)",
				Sources:  cli.EnvVars("SND_PRESET"),
				Required: true,
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
				Usage:   "Replica count override (0 = use preset's value)",
				Sources: cli.EnvVars("REPLICAS"),
			},
			&cli.StringSliceFlag{
				Name:  "override",
				Usage: "Seid config override KEY=VALUE, repeatable; merged into spec.template.spec.overrides",
			},
			&cli.StringSliceFlag{
				Name: "genesis-account",
				Usage: "Genesis account ADDR:BALANCE (e.g. sei1abc:1000000000usei), repeatable; " +
					"preset must include a genesis block",
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

	overrides, err := parseKVPairs(cmd.StringSlice("override"))
	if err != nil {
		return err
	}
	accounts, err := parseGenesisAccounts(cmd.StringSlice("genesis-account"))
	if err != nil {
		return err
	}

	p := provisionsnd.Params{
		Role:              cmd.String("role"),
		Name:              cmd.String("name"),
		Preset:            cmd.String("preset"),
		ChainID:           cmd.String("chain-id"),
		Image:             cmd.String("image"),
		Replicas:          int32(cmd.Int("replicas")),
		Overrides:         overrides,
		GenesisAccounts:   accounts,
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

func parseKVPairs(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(pairs))
	for _, kv := range pairs {
		idx := strings.IndexByte(kv, '=')
		if idx <= 0 {
			return nil, fmt.Errorf("--override %q must be KEY=VALUE", kv)
		}
		out[kv[:idx]] = kv[idx+1:]
	}
	return out, nil
}

func parseGenesisAccounts(specs []string) ([]seiv1alpha1.GenesisAccount, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	out := make([]seiv1alpha1.GenesisAccount, 0, len(specs))
	for _, s := range specs {
		idx := strings.IndexByte(s, ':')
		if idx <= 0 || idx == len(s)-1 {
			return nil, fmt.Errorf("--genesis-account %q must be ADDRESS:BALANCE", s)
		}
		out = append(out, seiv1alpha1.GenesisAccount{
			Address: s[:idx],
			Balance: s[idx+1:],
		})
	}
	return out, nil
}
