//go:build envtest

// Package envtest_test runs SeiNodeTask validation and reconcile checks
// against a real kube-apiserver started by controller-runtime's envtest
// harness. Build-tagged so default `go test ./...` skips it — the harness
// depends on a downloaded apiserver/etcd binary set (see Makefile
// `setup-envtest`).
//
// CEL XValidation rules (the spec union: exactly-one payload, kind/payload
// agreement, kind immutability) are enforced by the apiserver, not the fake
// client, so they can only be exercised here.
package envtest_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

var (
	testCli client.Client
	testCtx context.Context
)

func TestMain(m *testing.M) {
	logf.SetLogger(zap.New(zap.UseDevMode(true), zap.WriteTo(os.Stderr)))
	code, err := run(m)
	if err != nil {
		fmt.Fprintf(os.Stderr, "envtest run failed: %v\n", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func run(m *testing.M) (int, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return 1, fmt.Errorf("runtime.Caller failed; cannot resolve test paths")
	}
	pkgDir := filepath.Dir(thisFile)
	repoRoot := filepath.Clean(filepath.Join(pkgDir, "..", "..", "..", ".."))
	controllerCRDs := filepath.Join(repoRoot, "config", "crd")

	sch := clientgoscheme.Scheme
	utilruntime.Must(seiv1alpha1.AddToScheme(sch))

	testEnv := &envtest.Environment{
		CRDDirectoryPaths:     []string{controllerCRDs},
		ErrorIfCRDPathMissing: true,
		Scheme:                sch,
	}

	cfg, err := testEnv.Start()
	if err != nil {
		return 1, fmt.Errorf("starting envtest: %w", err)
	}
	defer func() {
		if stopErr := testEnv.Stop(); stopErr != nil {
			fmt.Fprintf(os.Stderr, "envtest stop: %v\n", stopErr)
		}
	}()

	cli, err := client.New(cfg, client.Options{Scheme: sch})
	if err != nil {
		return 1, fmt.Errorf("building test client: %w", err)
	}
	testCli = cli

	var cancel context.CancelFunc
	testCtx, cancel = context.WithCancel(context.Background())
	defer cancel()

	return m.Run(), nil
}
