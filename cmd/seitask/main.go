// Command seitask is the monolithic Workflow-Task primitive binary: one
// binary, multiple urfave/cli subcommands (keygen, provision-snd, …) that
// share the internal/taskruntime shared library. See
// docs/design/test-harness-lld.md.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/urfave/cli/v3"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/sei-protocol/sei-k8s-controller/internal/taskruntime"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	app := &cli.Command{
		Name:  "seitask",
		Usage: "Workflow Task primitives for the sei-k8s-controller test harness",
		Commands: []*cli.Command{
			newKeygenCommand(),
			newProvisionSNDCommand(),
			newRunnerCommand(),
			newUploadReportCommand(),
		},
	}

	if err := app.Run(ctx, os.Args); err != nil {
		// Subcommands wrap with taskruntime.Infra / taskruntime.Task so this
		// mapping reaches the right 0/1/2 exit code.
		log.Printf("seitask: %v", err)
		os.Exit(taskruntime.ExitCodeFor(err))
	}
}

// kubeClientFromEnv uses standard ctrl.GetConfig discovery (in-cluster SA
// → $KUBECONFIG → ~/.kube/config).
func kubeClientFromEnv() (client.Client, error) {
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return nil, taskruntime.Infra(fmt.Errorf("loading kubeconfig: %w", err))
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		return nil, taskruntime.Infra(fmt.Errorf("building client: %w", err))
	}
	return c, nil
}
