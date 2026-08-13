// Command sei-sidecar is the per-node sidecar: a task executor and HTTP API
// that the controller drives over kube-rbac-proxy. It runs as a native
// restartable sidecar container beside seid in every SeiNode pod.
//
// It ships as `serve` under a root command rather than as a bare binary because
// the controller renders `Command: []string{"seictl", "serve"}` into every pod
// spec. The image keeps a `seictl` symlink for that reason; see Dockerfile.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/sei-protocol/seilog"
	"github.com/urfave/cli/v3"
)

// destinations holds flag-bound values shared with the subcommands, mirroring
// the shape the seictl CLI used so serve.go's call sites are unchanged.
//
// home is the one that matters. It binds SEI_HOME, which the controller sets to
// the node's data-PVC mount. In seictl this flag lived on the root command and
// serve.go read it from here; the wiring is reproduced deliberately, because
// losing it does not fail — it silently relocates every write off the PVC.
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

	if err := cmd.Run(context.Background(), os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		_ = seilog.Close()
		os.Exit(1)
	}
}
