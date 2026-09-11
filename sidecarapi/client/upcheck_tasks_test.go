package client

import (
	"encoding/json"
	"testing"

	"github.com/sei-protocol/sei-k8s-controller/sidecarapi/wire"
)

// The wire body must carry the up-check under the key the sidecar decodes
// (upCheck/scheme/port/path); a nil up-check sends no params at all.
func TestUpCheckTasks_ToTaskRequest(t *testing.T) {
	check := &wire.UpCheck{Scheme: wire.UpCheckHTTP, Port: 26657, Path: "/status"}
	for _, tk := range []TaskBuilder{RestartSeidTask{UpCheck: check}, StopSeidTask{UpCheck: check}} {
		req := tk.ToTaskRequest()
		if req.Type != tk.TaskType() {
			t.Fatalf("%s: Type = %q", tk.TaskType(), req.Type)
		}
		if req.Params == nil {
			t.Fatalf("%s: Params nil", tk.TaskType())
		}
		raw, err := json.Marshal(req.Params)
		if err != nil {
			t.Fatal(err)
		}
		want := `{"upCheck":{"path":"/status","port":26657,"scheme":"http"}}`
		if string(raw) != want {
			t.Errorf("%s: params = %s, want %s", tk.TaskType(), raw, want)
		}
	}
	for _, tk := range []TaskBuilder{RestartSeidTask{}, StopSeidTask{}} {
		if req := tk.ToTaskRequest(); req.Params != nil {
			t.Errorf("%s: nil up-check must send no params, got %v", tk.TaskType(), *req.Params)
		}
	}
	if err := (StopSeidTask{UpCheck: &wire.UpCheck{Scheme: wire.UpCheckTCP, Port: 26656, Path: "/x"}}).Validate(); err == nil {
		t.Error("tcp with path must fail validation")
	}
}
