package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	snapshottypes "github.com/sei-protocol/sei-chain/sei-cosmos/snapshots/types"
	abci "github.com/sei-protocol/sei-chain/sei-tendermint/abci/types"
	"github.com/sei-protocol/sei-chain/sei-tendermint/rpc/coretypes"
)

// snapshotQueryJSON encodes a query result with the pinned RPC response types.
func snapshotQueryJSON(t *testing.T, value []byte) string {
	t.Helper()
	result := coretypes.ResultABCIQuery{Response: abci.ResponseQuery{Value: value, Height: 9000}}
	encoded, err := json.Marshal(&result)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// TestSnapshotClient_ListSnapshots queries a completed record through both RPC
// response formats and checks that its chunk hashes reach the caller intact.
func TestSnapshotClient_ListSnapshots(t *testing.T) {
	// The metadata is protobuf with two repeated bytes fields, independent of
	// SnapshotFromABCI, whose decoding this test exercises.
	advertised := abci.ResponseListSnapshots{Snapshots: []*abci.Snapshot{{
		Height: 42, Format: 1, Chunks: 2, Hash: []byte{5, 6},
		Metadata: []byte{0x0a, 0x02, 0x01, 0x02, 0x0a, 0x02, 0x03, 0x04},
	}}}
	value, err := json.Marshal(advertised)
	if err != nil {
		t.Fatal(err)
	}
	want := []snapshottypes.Snapshot{{
		Height: 42, Format: 1, Chunks: 2, Hash: []byte{5, 6},
		Metadata: snapshottypes.Metadata{ChunkHashes: [][]byte{{1, 2}, {3, 4}}},
	}}
	flat := snapshotQueryJSON(t, value)
	for _, format := range []string{"flat", "JSON-RPC"} {
		t.Run(format, func(t *testing.T) {
			response := flat
			if format == "JSON-RPC" {
				response = wrapResult(flat)
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/abci_query" || r.URL.Query().Get("path") != `"/app/snapshots"` {
					t.Errorf("snapshot request = %s %s", r.Method, r.URL)
				}
				_, _ = w.Write([]byte(response))
			}))
			defer srv.Close()

			rpcClient := NewClient(srv.URL, srv.Client())
			got, err := NewSnapshotClient(rpcClient).ListSnapshots(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("completed snapshots = %+v, want %+v", got, want)
			}
		})
	}
}

// TestSnapshotClient_EmptyCompletedList checks a healthy producer with no completed
// snapshots, so a successful empty result remains distinct from malformed replies.
func TestSnapshotClient_EmptyCompletedList(t *testing.T) {
	value, err := json.Marshal(abci.ResponseListSnapshots{Snapshots: []*abci.Snapshot{}})
	if err != nil {
		t.Fatal(err)
	}
	response := snapshotQueryJSON(t, value)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(response))
	}))
	defer srv.Close()

	got, err := NewSnapshotClient(NewClient(srv.URL, nil)).ListSnapshots(t.Context())
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("completed snapshots = %+v, %v; want an empty list", got, err)
	}
}

// TestSnapshotClient_RejectsMalformedResponses checks that failures in each
// response layer return an error rather than an apparently successful empty list.
func TestSnapshotClient_RejectsMalformedResponses(t *testing.T) {
	tests := []struct {
		name     string
		response string
		wantErr  string
	}{
		{
			name:     "malformed JSON",
			response: "not JSON",
			wantErr:  "decoding JSON response",
		},
		{
			name:     "missing response",
			response: `{}`,
			wantErr:  "no response",
		},
		{
			name:     "null response",
			response: `{"response":null}`,
			wantErr:  "no response",
		},
		{
			name:     "missing value",
			response: `{"response":{"code":0}}`,
			wantErr:  "no value",
		},
		{
			name:     "invalid base64 value",
			response: `{"response":{"value":"!"}}`,
			wantErr:  "decoding snapshot query response",
		},
		{
			name:     "query error",
			response: `{"response":{"code":12,"log":"snapshots unavailable"}}`,
			wantErr:  "code 12: snapshots unavailable",
		},
		{
			name:     "malformed snapshot JSON",
			response: snapshotQueryJSON(t, []byte("invalid")),
			wantErr:  "decoding completed snapshots",
		},
		{
			name:     "missing list",
			response: snapshotQueryJSON(t, []byte(`{}`)),
			wantErr:  "no snapshot list",
		},
		{
			name:     "null list",
			response: snapshotQueryJSON(t, []byte(`{"Snapshots":null}`)),
			wantErr:  "no snapshot list",
		},
		{
			name:     "null snapshot",
			response: snapshotQueryJSON(t, []byte(`{"Snapshots":[null]}`)),
			wantErr:  "null snapshot",
		},
		{
			name:     "invalid metadata protobuf",
			response: snapshotQueryJSON(t, []byte(`{"Snapshots":[{"Height":42,"Format":1,"Chunks":1,"Metadata":"/w=="}]}`)),
			wantErr:  "decoding metadata for snapshot 42",
		},
		{
			name:     "RPC error",
			response: `{"jsonrpc":"2.0","error":{"code":-32603,"message":"node restarting"}}`,
			wantErr:  "node restarting",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(test.response))
			}))
			defer srv.Close()

			got, err := NewSnapshotClient(NewClient(srv.URL, nil)).ListSnapshots(t.Context())
			if err == nil || !strings.Contains(err.Error(), test.wantErr) || got != nil {
				t.Fatalf("completed snapshots = %+v, %v; want error containing %q", got, err, test.wantErr)
			}
		})
	}
}

// TestSnapshotClient_HTTPFailure checks that an unavailable producer returns its
// HTTP failure to the caller without any completion records.
func TestSnapshotClient_HTTPFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "node unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	got, err := NewSnapshotClient(NewClient(srv.URL, nil)).ListSnapshots(t.Context())
	if got != nil || err == nil || !strings.Contains(err.Error(), "HTTP 503") {
		t.Fatalf("completed snapshots = %+v, %v; want HTTP 503", got, err)
	}
}

// TestSnapshotClient_CanceledRequest cancels after the producer receives the query
// and checks that the waiting caller receives context cancellation.
func TestSnapshotClient_CanceledRequest(t *testing.T) {
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := NewSnapshotClient(NewClient(srv.URL, nil)).ListSnapshots(ctx)
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("ListSnapshots error = %v, want context.Canceled", err)
	}
}

// TestSnapshotClient_NilClientUsesLocalEndpoint checks the default connection
// available to callers that have no existing RPC client.
func TestSnapshotClient_NilClientUsesLocalEndpoint(t *testing.T) {
	client := NewSnapshotClient(nil)
	if client.client.Endpoint() != DefaultEndpoint {
		t.Fatalf("endpoint = %q, want %q", client.client.Endpoint(), DefaultEndpoint)
	}
}
