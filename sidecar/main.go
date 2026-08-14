// Command sei-sidecar is the per-node sidecar: a task executor and HTTP API
// that the controller drives over kube-rbac-proxy. It runs as a native
// restartable sidecar container beside seid in every SeiNode pod.
//
// The controller renders no `Command` for the sidecar container, so the image's
// ENTRYPOINT is what runs it: `sei-sidecar serve`. `serve` is a subcommand
// rather than the root action so the binary keeps room for operator
// subcommands, and it is named in the ENTRYPOINT rather than left as a default
// because a bare invocation prints help and exits 0 — under
// restartPolicy: Always that is a container looping while reporting success.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/sei-protocol/seilog"
	"github.com/urfave/cli/v3"
)

// destinations holds flag-bound values the subcommands read.
//
// home binds SEI_HOME, which the controller sets to the node's data-PVC mount.
// It lives on the root command so every subcommand resolves the same directory,
// and losing that binding does not fail — it silently relocates every write off
// the PVC, which is why validateHome guards it.
var destinations = struct {
	home string
}{}

func main() {
	cmd := &cli.Command{
		Name:  "sei-sidecar",
		Usage: "Sei node sidecar: task executor and HTTP API",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "home",
				Sources: cli.EnvVars("SEI_HOME"),
				// No Value: an unset home must fail, not default. The previous
				// fallback was "/sei" while the controller mounts the data PVC
				// at $HOME/.sei, so a dropped SEI_HOME produced a running,
				// probe-passing sidecar writing genesis and config into an
				// empty directory. Required turns that into a startup error.
				Required:    true,
				Destination: &destinations.home,
				TakesFile:   true,
				Config:      cli.StringConfig{TrimSpace: true},
				Usage:       "seid home directory (the node's data volume)",
			},
		},
		Commands: []*cli.Command{&serveCmd},
	}

	cmd.Before = func(ctx context.Context, _ *cli.Command) (context.Context, error) {
		return ctx, validateHome(destinations.home)
	}

	if err := cmd.Run(context.Background(), os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		_ = seilog.Close()
		os.Exit(1)
	}
}

// validateHome rejects a set-but-empty home directory.
//
// The flag's Required only tests whether a value was supplied, and SEI_HOME=""
// supplies one. Without this check the sidecar starts and resolves every path
// relative to its working directory — measured, it creates config/, data/ and
// sidecar.db there and serves normally — which is the silent wrong-directory
// failure Required is there to prevent.
func validateHome(home string) error {
	if strings.TrimSpace(home) == "" {
		return errors.New("SEI_HOME is set but empty; it must name the node's data " +
			"volume, or every path resolves relative to the working directory")
	}
	return nil
}
