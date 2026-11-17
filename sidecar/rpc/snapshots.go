package rpc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"

	snapshottypes "github.com/sei-protocol/sei-chain/sei-cosmos/snapshots/types"
	abci "github.com/sei-protocol/sei-chain/sei-tendermint/abci/types"
)

// SnapshotClient queries completed snapshots from a running node.
type SnapshotClient struct {
	client *Client
}

// NewSnapshotClient uses client with its configured transport and timeout.
// A nil client selects the default local RPC connection.
func NewSnapshotClient(client *Client) *SnapshotClient {
	if client == nil {
		client = NewClient("", nil)
	}
	return &SnapshotClient{client: client}
}

// ListSnapshots returns completion records advertised by the node in producer order.
// An empty list means the query succeeded with no completed snapshots.
// Request failures and invalid metadata encoding return an error; the uploader
// validates snapshot heights, formats and hashes before publication.
func (c *SnapshotClient) ListSnapshots(ctx context.Context) ([]snapshottypes.Snapshot, error) {
	value, err := c.queryCompletedSnapshots(ctx)
	if err != nil {
		return nil, err
	}
	return decodeCompletedSnapshots(value)
}

// queryCompletedSnapshots fetches the producer list and validates the ABCI reply.
func (c *SnapshotClient) queryCompletedSnapshots(ctx context.Context) ([]byte, error) {
	query := url.Values{"path": {`"/app/snapshots"`}}
	raw, err := c.client.Get(ctx, "/abci_query?"+query.Encode())
	if err != nil {
		return nil, fmt.Errorf("querying completed snapshots: %w", err)
	}

	// ResponseQuery owns protobuf JSON decoding, including base64 values and
	// decimal strings for heights. The pointer distinguishes a missing response.
	var result struct {
		Response *abci.ResponseQuery `json:"response"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("decoding snapshot query response: %w", err)
	}
	if result.Response == nil {
		return nil, fmt.Errorf("snapshot query returned no response")
	}
	if result.Response.Code != 0 {
		return nil, fmt.Errorf("snapshot query failed with code %d: %s", result.Response.Code, result.Response.Log)
	}
	if len(result.Response.Value) == 0 {
		return nil, fmt.Errorf("snapshot query returned no value")
	}

	return result.Response.Value, nil
}

// decodeCompletedSnapshots translates the producer list and its SDK metadata.
func decodeCompletedSnapshots(value []byte) ([]snapshottypes.Snapshot, error) {
	var listed abci.ResponseListSnapshots
	if err := json.Unmarshal(value, &listed); err != nil {
		return nil, fmt.Errorf("decoding completed snapshots: %w", err)
	}
	if listed.Snapshots == nil {
		return nil, fmt.Errorf("snapshot query returned no snapshot list")
	}

	completed := make([]snapshottypes.Snapshot, 0, len(listed.Snapshots))
	for i, snapshot := range listed.Snapshots {
		if snapshot == nil {
			return nil, fmt.Errorf("snapshot query returned a null snapshot at index %d", i)
		}
		decoded, err := snapshottypes.SnapshotFromABCI(snapshot)
		if err != nil {
			return nil, fmt.Errorf("decoding metadata for snapshot %d format %d: %w", snapshot.Height, snapshot.Format, err)
		}
		completed = append(completed, decoded)
	}
	return completed, nil
}
