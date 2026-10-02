// Command evmdigest-comparator compares the EVM logical state digest of every
// node in a FlatKV migration cell — migrating nodes plus memIAVL reserves —
// and serves the results as Prometheus metrics on /metrics. Each round it
// picks one height below the lowest committed tip, fans an evm-digest sidecar
// task out to every node, and latches a diverged gauge when the scan reports
// disagree.
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
		slog.Error("evmdigest-comparator exited", "error", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "/etc/evmdigest-comparator/config.yaml", "Path to the chains config file.")
	metricsAddr := flag.String("metrics-bind-address", ":8080", "Address serving /metrics and /healthz.")
	roundInterval := flag.Duration("round-interval", 10*time.Minute, "Pause between a chain's digest rounds.")
	heightLag := flag.Int64("height-lag", 20, "Blocks below the lowest committed tip a round scans at.")
	taskPoll := flag.Duration("task-poll", 15*time.Second, "How often a running evm-digest task is polled.")
	scanTimeout := flag.Duration("scan-timeout", 30*time.Minute, "Bounds one evm-digest task attempt.")
	scanAttempts := flag.Int("scan-attempts", 3,
		"Submissions per node per round; changelog scans can fail transiently mid-record.")
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

	// Only in-cluster kube-rbac-proxy endpoints receive the ServiceAccount token;
	// url endpoints are called without credentials.
	inClusterDoer := &http.Client{Transport: sidecartransport.New(sidecartransport.Config{TokenPath: *tokenPath})}
	externalDoer := &http.Client{}
	comparator := &evmdigestcompare.Comparator{
		Chains: cfg.Chains,
		NewSource: func(e evmdigestcompare.Endpoint) (evmdigestcompare.Source, error) {
			doer := externalDoer
			if e.InCluster() {
				doer = inClusterDoer
			}
			client, err := sidecar.NewSidecarClient(e.BaseURL(),
				sidecar.WithHTTPDoer(doer), sidecar.WithTimeout(*requestTimeout))
			if err != nil {
				return nil, err
			}
			return &evmdigestcompare.SidecarSource{
				Client:      client,
				TaskPoll:    *taskPoll,
				ScanTimeout: *scanTimeout,
				Attempts:    *scanAttempts,
			}, nil
		},
		Metrics:       metrics,
		RoundInterval: *roundInterval,
		HeightLag:     *heightLag,
		Log:           log,
		Now:           time.Now,
	}

	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	srv := &http.Server{Addr: *metricsAddr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()
	log.Info("evmdigest-comparator started", "chains", len(cfg.Chains), "metrics", *metricsAddr)

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
