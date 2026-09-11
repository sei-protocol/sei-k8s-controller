package rpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	seiconfig "github.com/sei-protocol/sei-config"
)

const heightTimeout = 2 * time.Second

// DefaultEVMEndpoint is the local EVM JSON-RPC address.
var DefaultEVMEndpoint = fmt.Sprintf("http://localhost:%d", seiconfig.PortEVMHTTP)

// HeightReader reports the committed height of the co-located seid from
// whichever local RPC the node's mode serves: CometBFT /status first, then
// the EVM JSON-RPC eth_blockNumber, which is the only listener in EVM-only
// mode. A node that answers neither is unreadable, not at height zero.
type HeightReader struct {
	comet      *StatusClient
	evm        string
	httpClient HTTPDoer
}

// NewHeightReader targets the given CometBFT and EVM endpoints. Pass "" for
// either to use the loopback default, and nil for the default HTTP client.
func NewHeightReader(cometEndpoint, evmEndpoint string, httpClient HTTPDoer) *HeightReader {
	if evmEndpoint == "" {
		evmEndpoint = DefaultEVMEndpoint
	}
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	return &HeightReader{
		comet:      NewStatusClient(cometEndpoint, httpClient),
		evm:        evmEndpoint,
		httpClient: httpClient,
	}
}

// CommittedHeight returns the committed height, or an error joining both
// sources' failures when neither answers.
func (r *HeightReader) CommittedHeight(ctx context.Context) (int64, error) {
	h, cometErr := r.comet.LatestHeight(ctx)
	if cometErr == nil {
		return h, nil
	}
	h, evmErr := r.evmBlockNumber(ctx)
	if evmErr == nil {
		return h, nil
	}
	return 0, errors.Join(
		fmt.Errorf("cometbft: %w", cometErr),
		fmt.Errorf("evm: %w", evmErr),
	)
}

func (r *HeightReader) evmBlockNumber(ctx context.Context) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, heightTimeout)
	defer cancel()

	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.evm, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return 0, fmt.Errorf("HTTP %d: %s", resp.StatusCode, msg)
	}

	var env struct {
		Result string    `json:"result"`
		Error  *rpcError `json:"error,omitempty"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&env); err != nil {
		return 0, fmt.Errorf("decoding eth_blockNumber response: %w", err)
	}
	if env.Error != nil {
		return 0, fmt.Errorf("JSON-RPC error: %s (code %d)", env.Error.Message, env.Error.Code)
	}
	hex := strings.TrimPrefix(env.Result, "0x")
	if hex == "" {
		return 0, fmt.Errorf("empty eth_blockNumber result %q", env.Result)
	}
	h, err := strconv.ParseInt(hex, 16, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing eth_blockNumber %q: %w", env.Result, err)
	}
	return h, nil
}
