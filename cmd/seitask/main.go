// Command seitask is the monolithic Workflow-Task primitive binary: one
// binary, multiple urfave/cli subcommands (keygen, provision-snd, …) that
// share the internal/taskruntime shared library. See
// https://github.com/sei-protocol/bdchatham-designs/blob/main/designs/test-harness/test-harness-lld.md.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/urfave/cli/v3"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/taskruntime"
)

// taskScheme is the controller-runtime client scheme for every seitask
// subcommand: builtin K8s types + sei.io/v1alpha1 (SeiNetwork,
// SeiNodeTask, SeiNode) so typed Create/Get round-trips work. Chaos Mesh
// CRs are read via unstructured so they're not registered here.
var taskScheme = func() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(seiv1alpha1.AddToScheme(s))
	return s
}()

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
	c, err := client.New(cfg, client.Options{Scheme: taskScheme})
	if err != nil {
		return nil, taskruntime.Infra(fmt.Errorf("building client: %w", err))
	}
	return c, nil
}
