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
	"path/filepath"
	"strings"

	"github.com/sei-protocol/seilog"
	"github.com/urfave/cli/v3"
)

func main() {
	cmd := newRootCommand()
	cmd.Commands = []*cli.Command{&serveCmd}

	if err := cmd.Run(context.Background(), os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		_ = seilog.Close()
		os.Exit(1)
	}
}

// newRootCommand builds the root command without its subcommands, so a test can
// drive the same flag wiring production runs.
func newRootCommand() *cli.Command {
	return &cli.Command{
		Name:  "sei-sidecar",
		Usage: "Sei node sidecar: task executor and HTTP API",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "home",
				Sources: cli.EnvVars("SEI_HOME"),
				// No Value: a wrong home is silent, because every path the
				// sidecar writes is resolved against it.
				Required:  true,
				TakesFile: true,
				Config:    cli.StringConfig{TrimSpace: true},
				Usage:     "seid home directory (the node's data volume)",
				// Validation belongs here, not in a root Before hook. urfave
				// runs Before hooks ahead of its own required-flag check, so a
				// hook would shadow that check and report an unset SEI_HOME as
				// though it were set-but-empty. A flag Action runs only for
				// flags that were actually set, which leaves the unset case to
				// the required-flag check that describes it correctly.
				Action: func(_ context.Context, cmd *cli.Command, home string) error {
					if err := validateHome(home); err != nil {
						return err
					}
					// Normalize before any subcommand reads it: two consumers
					// take the value raw rather than through filepath.Join —
					// tasks.NewSnapshotUploader's os.CreateTemp root and the
					// gentx generator's cfg.SetRoot.
					return cmd.Set("home", filepath.Clean(home))
				},
			},
		},
	}
}

// validateHome rejects a home directory that would silently relocate every
// write off the node's data volume.
//
// Two shapes get past the flag's Required, which only tests that a value was
// supplied. An empty value resolves every path against the working directory,
// and so does any relative value — the harm is identical, so both are refused
// rather than only the one that prompted the check.
func validateHome(home string) error {
	trimmed := strings.TrimSpace(home)
	if trimmed == "" {
		return errors.New("SEI_HOME is set but empty; it must name the node's data " +
			"volume, or every path resolves relative to the working directory")
	}
	if !filepath.IsAbs(trimmed) {
		return fmt.Errorf("SEI_HOME must be an absolute path; got %q, which resolves "+
			"relative to the working directory rather than the node's data volume", home)
	}
	return nil
}
