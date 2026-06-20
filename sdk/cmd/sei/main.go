// Command sei is the thin CLI shell over sei-sdk (WS-E LLD §6.2). It is NOT a
// seictl replacement: a single `up` subcommand provisions a network + fleet and
// prints the fleet EVM endpoints in one process, dogfooding the SDK surface and
// giving a migrating harness a shell drop-in for the parts that don't need Go.
//
// Standalone, cross-invocation verbs (teardown by name, endpoints by name)
// require reading existing resources back into a handle, which the MVP does not
// wire — the SDK's payoff is the in-process path where a Go harness holds the
// *Network/*Fleet across provision, assert, and teardown (see example_test.go).
// `up` here registers a deferred teardown so the CLI cleans up after itself.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sei-protocol/sei-k8s-controller/sdk/sei"
	_ "github.com/sei-protocol/sei-k8s-controller/sdk/sei/provider/k8s" // registers "k8s"
)

// teardownTimeout bounds the deferred cleanup deletes. They run on a fresh
// context, so a SIGINT/deadline that canceled the provisioning ctx doesn't skip
// them — but they still need a deadline of their own against a wedged apiserver.
const teardownTimeout = 30 * time.Second

func main() {
	if len(os.Args) < 2 || os.Args[1] != "up" {
		fmt.Fprintln(os.Stderr, "usage: sei up [flags]")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := up(ctx, os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "sei:", err)
		os.Exit(exitCode(err))
	}
}

// exitCode maps the SDK error Class onto a stable process exit code so a shell
// harness can branch on cause without parsing the message.
func exitCode(err error) int {
	switch {
	case sei.IsTimeout(err):
		return 3
	case sei.IsFailed(err):
		return 4
	default:
		return 1
	}
}

func up(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	var (
		name       = fs.String("name", "", "SeiNetwork name (required)")
		namespace  = fs.String("namespace", "", "namespace (default: provider default)")
		chainID    = fs.String("chain-id", "", "genesis chain ID (required)")
		image      = fs.String("image", "", "seid image (required)")
		validators = fs.Int("validators", 1, "genesis validator count")
		prefix     = fs.String("fleet-prefix", "", "follower name prefix (required)")
		followers  = fs.Int("followers", 1, "follower count")
		keep       = fs.Bool("keep", false, "do not tear down on exit")
		timeout    = fs.Duration("timeout", 30*time.Minute, "overall deadline")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *prefix == "" {
		return fmt.Errorf("up: --fleet-prefix is required")
	}

	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	// Empty name: the SDK owns flavor resolution (explicit -> SEI_PROVIDER ->
	// presence detection). Reading SEI_PROVIDER here would short-circuit that
	// chain and drop presence detection.
	c, err := sei.Open(ctx, "")
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	net, err := c.ProvisionNetwork(ctx, sei.NetworkSpec{
		Name: *name, Namespace: *namespace, Preset: "genesis-chain",
		ChainID: *chainID, Image: *image, Replicas: *validators,
	})
	if err != nil {
		return err
	}
	if !*keep {
		// Fresh ctx, NOT the provisioning ctx: on a SIGINT/SIGTERM or deadline exit
		// the provisioning ctx is already canceled, and Teardown's Delete would
		// short-circuit on ctx.Err() — skipping cleanup exactly when it's needed.
		defer func() {
			tdCtx, cancel := context.WithTimeout(context.Background(), teardownTimeout)
			defer cancel()
			_ = net.Teardown(tdCtx)
		}()
	}

	fleet, err := c.ProvisionFleet(ctx, net, sei.FleetSpec{
		NamePrefix: *prefix, Namespace: *namespace, Preset: "rpc",
		Image: *image, Replicas: *followers,
	})
	if err != nil {
		return err
	}
	if !*keep {
		// Fresh ctx (see net.Teardown above): the deferred fleet teardown must
		// outlive a canceled provisioning ctx.
		defer func() {
			tdCtx, cancel := context.WithTimeout(context.Background(), teardownTimeout)
			defer cancel()
			_ = fleet.Teardown(tdCtx)
		}()
	}

	for _, u := range fleet.Endpoints().EVMRPCList() {
		fmt.Println(u)
	}
	return nil
}
