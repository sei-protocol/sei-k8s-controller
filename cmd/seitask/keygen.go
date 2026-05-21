package main

import (
	"context"
	"log"

	"github.com/urfave/cli/v3"

	"github.com/sei-protocol/sei-k8s-controller/internal/seitask/keygen"
	"github.com/sei-protocol/sei-k8s-controller/internal/taskruntime"
)

func newKeygenCommand() *cli.Command {
	return &cli.Command{
		Name: "keygen",
		Usage: "Generate a fresh BIP-39 mnemonic and cosmos secp256k1 keypair, write it to a " +
			"per-run Secret, and stamp the address into workflow-vars",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:     "key-name",
				Aliases:  []string{"k"},
				Usage:    "Logical identity name; Secret is named <key-name>-<workflow-name>",
				Sources:  cli.EnvVars("KEY_NAME"),
				Value:    "admin",
				Required: false,
			},
		},
		Action: runKeygen,
	}
}

func runKeygen(ctx context.Context, cmd *cli.Command) error {
	c, err := kubeClientFromEnv()
	if err != nil {
		return err
	}
	wf, err := taskruntime.LoadWorkflowIdentity(ctx, c)
	if err != nil {
		return err
	}

	res, err := keygen.Run(ctx, c, keygen.Params{
		KeyName:  cmd.String("key-name"),
		Workflow: wf,
	})
	if err != nil {
		// Stamp EXIT_REASON so upload-report can recover the failure class.
		taskruntime.WriteExitReason(ctx, c, wf, err)
		return err
	}
	taskruntime.WriteExitReason(ctx, c, wf, nil)
	log.Printf("keygen: created Secret %q with address %s", res.SecretName, res.Address)
	return nil
}
