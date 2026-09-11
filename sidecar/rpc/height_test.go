package rpc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func evmServer(t *testing.T, result string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":` + result + `}`))
	}))
}

func downServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	return srv
}

func TestHeightReader_PrefersCometBFT(t *testing.T) {
	comet := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(wrapResult(`{"sync_info":{"latest_block_height":"42","catching_up":false}}`)))
	}))
	defer comet.Close()
	evm := evmServer(t, `"0x7"`)
	defer evm.Close()

	h, err := NewHeightReader(comet.URL, evm.URL, nil).CommittedHeight(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if h != 42 {
		t.Errorf("height = %d, want 42", h)
	}
}

func TestHeightReader_FallsBackToEVM(t *testing.T) {
	comet := downServer(t)
	defer comet.Close()
	evm := evmServer(t, `"0x1a"`)
	defer evm.Close()

	h, err := NewHeightReader(comet.URL, evm.URL, nil).CommittedHeight(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if h != 26 {
		t.Errorf("height = %d, want 26", h)
	}
}

func TestHeightReader_BothDown(t *testing.T) {
	comet := downServer(t)
	defer comet.Close()
	evm := downServer(t)
	defer evm.Close()

	_, err := NewHeightReader(comet.URL, evm.URL, nil).CommittedHeight(context.Background())
	if err == nil {
		t.Fatal("expected error when neither RPC answers")
	}
}

func TestHeightReader_BadEVMResult(t *testing.T) {
	comet := downServer(t)
	defer comet.Close()
	evm := evmServer(t, `"0x"`)
	defer evm.Close()

	if _, err := NewHeightReader(comet.URL, evm.URL, nil).CommittedHeight(context.Background()); err == nil {
		t.Fatal("expected error for empty hex result")
	}
}
