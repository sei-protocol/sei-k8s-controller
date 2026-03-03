package node

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const defaultSidecarPort int32 = 7777

// StatusResponse mirrors the sidecar's GET /status JSON shape.
// Defined here to avoid importing the sidecar module.
type StatusResponse struct {
	Status   string      `json:"status"`
	LastTask *TaskResult `json:"lastTask,omitempty"`
}

// TaskResult records the outcome of a non-scheduled task.
type TaskResult struct {
	Type  string `json:"type"`
	Error string `json:"error,omitempty"`
}

// TaskRequest is the JSON body for POST /task.
type TaskRequest struct {
	Type   string         `json:"type"`
	Params map[string]any `json:"params,omitempty"`
}

// SidecarClient is a typed HTTP client for the sei-sidecar running
// inside a SeiNode pod. The controller uses this to poll status and
// submit tasks during reconciliation.
type SidecarClient struct {
	baseURL    string
	httpClient *http.Client
}

// NewSidecarClient constructs a client targeting the sidecar via headless
// service DNS. The URL format is deterministic and works before the pod
// reports ready — exactly what the controller needs during bootstrap.
func NewSidecarClient(nodeName, namespace string, port int32) *SidecarClient {
	if port == 0 {
		port = defaultSidecarPort
	}
	return &SidecarClient{
		baseURL: fmt.Sprintf("http://%s-0.%s.%s.svc.cluster.local:%d",
			nodeName, nodeName, namespace, port),
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// ErrSidecarBusy is returned when the sidecar rejects a task because
// another task is already running (HTTP 409).
var ErrSidecarBusy = fmt.Errorf("sidecar is busy")

// Status queries the sidecar's current lifecycle state.
func (c *SidecarClient) Status(ctx context.Context) (*StatusResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/status", nil)
	if err != nil {
		return nil, fmt.Errorf("creating status request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("querying sidecar status: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("sidecar status returned %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}

	var status StatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return nil, fmt.Errorf("decoding status response: %w", err)
	}
	return &status, nil
}

// SubmitTask sends a task to the sidecar for execution.
// Returns nil on HTTP 202 (accepted), ErrSidecarBusy on 409, and a
// descriptive error for any other non-202 response.
func (c *SidecarClient) SubmitTask(ctx context.Context, task TaskRequest) error {
	body, err := json.Marshal(task)
	if err != nil {
		return fmt.Errorf("marshaling task request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/task", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("creating task request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("submitting task to sidecar: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusAccepted:
		return nil
	case http.StatusConflict:
		return ErrSidecarBusy
	default:
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("sidecar task submission returned %d: %s", resp.StatusCode, bytes.TrimSpace(respBody))
	}
}

// Healthz checks whether the sidecar has completed bootstrap (mark-ready).
// Returns (true, nil) for HTTP 200, (false, nil) for HTTP 503, and
// (false, error) for network failures or unexpected status codes.
func (c *SidecarClient) Healthz(ctx context.Context) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/healthz", nil)
	if err != nil {
		return false, fmt.Errorf("creating healthz request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("querying sidecar healthz: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusServiceUnavailable:
		return false, nil
	default:
		body, _ := io.ReadAll(resp.Body)
		return false, fmt.Errorf("sidecar healthz returned %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}
}
