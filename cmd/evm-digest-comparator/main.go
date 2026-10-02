// Command evm-digest-comparator has every node in a group scan its full EVM
// logical state at one shared height, compares the digests, and serves the
// results as Prometheus metrics on /metrics.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/sei-protocol/sei-k8s-controller/internal/evmdigestcompare"
	"github.com/sei-protocol/sei-k8s-controller/internal/sidecartransport"
	sidecar "github.com/sei-protocol/sei-k8s-controller/sidecarapi/client"
)

func main() {
	if err := run(); err != nil {
		slog.Error("evm-digest-comparator exited", "error", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "/etc/evm-digest-comparator/config.yaml", "Path to the groups config file.")
	metricsAddr := flag.String("metrics-bind-address", ":8080", "Address serving /metrics and /healthz.")
	heightLag := flag.Int64("height-lag", 50, "Blocks below the group's lowest committed tip each round scans at.")
	roundInterval := flag.Duration("round-interval", 10*time.Minute,
		"Minimum time between the starts of two rounds of a group.")
	taskPollInterval := flag.Duration("task-poll-interval", 15*time.Second, "How often a running scan is polled.")
	scanTimeout := flag.Duration("scan-timeout", 3*time.Hour, "Longest one node's scan may run before it is cancelled.")
	requestTimeout := flag.Duration("request-timeout", 30*time.Second, "Timeout for one sidecar request.")
	tokenPath := flag.String("token-path", sidecartransport.DefaultServiceAccountTokenPath,
		"ServiceAccount token presented to in-cluster sidecars' kube-rbac-proxy.")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	cfg, err := evmdigestcompare.LoadConfig(*configPath)
	if err != nil {
		return err
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	metrics := evmdigestcompare.NewMetrics(reg)

	doer := &http.Client{Transport: sidecartransport.New(sidecartransport.Config{TokenPath: *tokenPath})}
	comparator := &evmdigestcompare.Comparator{
		Groups: cfg.Groups,
		NewSource: func(n evmdigestcompare.Node) (evmdigestcompare.Source, error) {
			return sidecar.NewSidecarClient(n.BaseURL(), sidecar.WithHTTPDoer(doer), sidecar.WithTimeout(*requestTimeout))
		},
		Metrics:          metrics,
		HeightLag:        *heightLag,
		RoundInterval:    *roundInterval,
		TaskPollInterval: *taskPollInterval,
		ScanTimeout:      *scanTimeout,
		Log:              log,
		Now:              time.Now,
	}

	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	srv := &http.Server{Addr: *metricsAddr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()
	log.Info("evm-digest-comparator started", "groups", len(cfg.Groups), "metrics", *metricsAddr)

	runErr := make(chan error, 1)
	go func() { runErr <- comparator.Run(ctx) }()

	select {
	case err := <-serveErr:
		stop()
		return err
	case err := <-runErr:
		if err != nil {
			return err
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
