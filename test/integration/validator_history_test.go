//go:build integration

package integration

import (
	"context"
	"fmt"
	"net/http"
	"os/signal"
	"syscall"
	"testing"
	"time"

	seiconfig "github.com/sei-protocol/sei-config"
	"golang.org/x/net/websocket"

	"github.com/sei-protocol/sei-k8s-controller/internal/keygen"
	"github.com/sei-protocol/sei-k8s-controller/sdk/sei"
)

// TestNightlyValidatorHistory repeats the steady-state workload with history,
// transaction indexing and both EVM listeners enabled on the signing nodes.
func TestNightlyValidatorHistory(t *testing.T) {
	requireCluster(t)
	id := runChainID(mustEnv(t, "SEI_CHAIN_ID")) + "-history"
	if len(rpcNodeName(id, 1)+"-0") > 63 {
		t.Fatalf("chain ID %q is too long for the RPC pod label", id)
	}
	root, err := keygen.DeriveEVM()
	if err != nil {
		t.Fatalf("derive sei-load funding account: %v", err)
	}
	s := spec{
		chainID: id, runID: id,
		namespace:  envOr("SEI_NAMESPACE", ""),
		seidImage:  mustEnv(t, seidImageEnv),
		validators: 4, rpcNodes: 2,
		seiloadImage:       mustEnv(t, "SEILOAD_IMAGE"),
		seiloadCommit:      envOr("SEILOAD_COMMIT_ID", ""),
		seiloadProfile:     "nightly_steady_state",
		seiloadWorkload:    "validator-history",
		durationMin:        30,
		minReceiptCoverage: 0.99,
		accounts:           []sei.GenesisAccount{{Address: root.Address, Balance: steadyStateRootBalance}},
		seiloadRootKey:     root.PrivateKeyHex,
		storageConfig: mergeConfig(memiavlStorageConfig, map[string]string{
			"storage.state_store.enable":      "true",
			"storage.state_store.keep_recent": "100000",
			"chain.min_retain_blocks":         "100000",
			"tx_index.indexer":                "kv",
			"evm.http_enabled":                "true",
			"evm.ws_enabled":                  "true",
		}),
		// Match the steady-state follower settings so the validator config
		// is the variable in the comparison.
		rpcConfig: map[string]string{
			"evm.worker_pool_size":  "32",
			"evm.worker_queue_size": "4000",
			"evm.max_tx_pool_txs":   "10000",
		},
		rpcConfigValues: []sei.ConfigValue{
			{FileName: "app.toml", Key: "evm.rate_limiting_enabled", Value: false},
		},
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Duration(s.durationMin+80)*time.Minute)
	defer cancel()
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM)
	defer stop()
	c := openClient(ctx, t)
	cs := clientset(t)
	ch, err := provision(ctx, t, c, s)
	cleanupChain(t, ch)
	if err != nil {
		t.Fatalf("provision %s: %v", id, err)
	}

	// Take a reference from the receipt follower before sei-load spends
	// from the root account. All validators must still serve that balance
	// at the same height after the load has finished.
	hc := &http.Client{Timeout: 10 * time.Second}
	height := mustLatestHeight(ctx, t, hc, ch.rpcNodes[1].TendermintRPC(), "pre-load") - 1
	ns := ch.network.Namespace()
	want := historicalBalanceWS(ctx, t, rpcNodeName(id, 1), ns, root.EVMAddress, height)
	runSeiload(ctx, t, cs, ch, s)
	for i := range s.validators {
		node := validatorName(id, i)
		t.Run(node, func(t *testing.T) {
			host := node + "." + ns + ".svc"
			probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			if err := sei.WaitEVMServing(probeCtx, hc, fmt.Sprintf("http://%s:%d", host, seiconfig.PortEVMHTTP)); err != nil {
				t.Fatal(err)
			}
			type status struct {
				NodeInfo struct {
					Other struct {
						TxIndex string `json:"tx_index"`
					} `json:"other"`
				} `json:"node_info"`
			}
			var body struct {
				status
				Result *status `json:"result"`
			}
			if !getJSONInto(probeCtx, hc, fmt.Sprintf("http://%s:%d/status", host, seiconfig.PortRPC), &body) {
				t.Fatal("read Tendermint status")
			}
			info := body.status
			if body.Result != nil {
				info = *body.Result
			}
			if info.NodeInfo.Other.TxIndex != "on" {
				t.Errorf("Tendermint transaction index = %q, want on", info.NodeInfo.Other.TxIndex)
			}
			if got := historicalBalanceWS(probeCtx, t, node, ns, root.EVMAddress, height); got != want {
				t.Errorf("balance at height %d = %s, want %s", height, got, want)
			}
		})
	}
}

func historicalBalanceWS(ctx context.Context, t *testing.T, node, ns, address string, height int64) string {
	t.Helper()
	host := node + "." + ns + ".svc"
	endpoint := fmt.Sprintf("ws://%s:%d", host, seiconfig.PortEVMWS)
	cfg, err := websocket.NewConfig(endpoint, "http://"+host)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := cfg.DialContext(ctx)
	if err != nil {
		t.Fatalf("connect to %s: %v", endpoint, err)
	}
	defer func() { _ = conn.Close() }()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	request := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "eth_getBalance", "params": []string{address, fmt.Sprintf("0x%x", height)}}
	if err := websocket.JSON.Send(conn, request); err != nil {
		t.Fatalf("send balance query to %s: %v", node, err)
	}
	var reply struct {
		Result string `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := websocket.JSON.Receive(conn, &reply); err != nil {
		t.Fatalf("read balance from %s: %v", node, err)
	}
	if reply.Error != nil {
		t.Fatalf("%s eth_getBalance at height %d: %s", node, height, reply.Error.Message)
	}
	if reply.Result == "" {
		t.Fatalf("%s returned an empty balance at height %d", node, height)
	}
	return reply.Result
}
