// Command hashlog-comparator compares the hash logs of FlatKV-migrating
// SeiNodes against memIAVL-only reserve nodes and serves the results as
// Prometheus metrics on /metrics.
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

	"github.com/sei-protocol/sei-k8s-controller/internal/hashlogcompare"
	"github.com/sei-protocol/sei-k8s-controller/internal/sidecartransport"
	sidecar "github.com/sei-protocol/sei-k8s-controller/sidecarapi/client"
)

func main() {
	if err := run(); err != nil {
		slog.Error("hashlog-comparator exited", "error", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "/etc/hashlog-comparator/config.yaml", "Path to the pairs config file.")
	metricsAddr := flag.String("metrics-bind-address", ":8080", "Address serving /metrics and /healthz.")
	pollInterval := flag.Duration("poll-interval", 5*time.Second, "How often each pair is polled.")
	requestTimeout := flag.Duration("request-timeout", 30*time.Second, "Timeout for one sidecar request.")
	tokenPath := flag.String("token-path", sidecartransport.DefaultServiceAccountTokenPath,
		"ServiceAccount token presented to in-cluster sidecars' kube-rbac-proxy.")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	cfg, err := hashlogcompare.LoadConfig(*configPath)
	if err != nil {
		return err
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	metrics := hashlogcompare.NewMetrics(reg)

	// Only in-cluster kube-rbac-proxy endpoints receive the ServiceAccount token;
	// url endpoints are called without credentials.
	inClusterDoer := &http.Client{Transport: sidecartransport.New(sidecartransport.Config{TokenPath: *tokenPath})}
	externalDoer := &http.Client{}
	comparator := &hashlogcompare.Comparator{
		Pairs: cfg.Pairs,
		NewSource: func(e hashlogcompare.Endpoint) (hashlogcompare.Source, error) {
			doer := externalDoer
			if e.InCluster() {
				doer = inClusterDoer
			}
			return sidecar.NewSidecarClient(e.BaseURL(), sidecar.WithHTTPDoer(doer), sidecar.WithTimeout(*requestTimeout))
		},
		Metrics:      metrics,
		PollInterval: *pollInterval,
		Log:          log,
		Now:          time.Now,
	}

	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	srv := &http.Server{Addr: *metricsAddr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()
	log.Info("hashlog-comparator started", "pairs", len(cfg.Pairs), "metrics", *metricsAddr)

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
