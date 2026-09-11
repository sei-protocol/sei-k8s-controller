package planner

import (
	"fmt"
	"maps"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/platform"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

// AutobahnConfigPath is where configure-genesis lands autobahn.json and what
// config.toml's autobahn-config-file points at.
const AutobahnConfigPath = platform.DataDir + "/config/autobahn.json"

// Files and keys the consensus overlay owns in seid's TOML.
const (
	consensusConfigFile   = "config.toml"
	consensusAppFile      = "app.toml"
	keyAutobahnConfigFile = "autobahn-config-file"
	keyEnable             = "enable"
)

// consensusOverlay renders spec.consensus as the seid keys the engine needs,
// in the same merge-patch shape as configValues so it rides the overlay
// splice after base regeneration. Tendermint renders nothing. EVM-only also
// closes the CometBFT RPC, REST, gRPC and gRPC-web listeners seid would
// otherwise open for an application it does not run.
func consensusOverlay(node *seiv1alpha1.SeiNode) *task.ConfigPatchTask {
	c := node.Spec.Consensus
	if !c.IsAutobahn() {
		return nil
	}
	configTOML := map[string]any{
		keyAutobahnConfigFile: AutobahnConfigPath,
	}
	patch := &task.ConfigPatchTask{Files: map[string]map[string]any{
		consensusConfigFile: configTOML,
	}}
	if !c.IsEvmOnly() {
		return patch
	}
	configTOML["evm-only"] = true
	configTOML["rpc"] = map[string]any{"laddr": ""}
	patch.Files[consensusAppFile] = map[string]any{
		"api":      map[string]any{keyEnable: false},
		"grpc":     map[string]any{keyEnable: false},
		"grpc-web": map[string]any{keyEnable: false},
	}
	return patch
}

// mergeOverlayPatches layers controller-owned engine keys over the user's
// configValues. A user entry on a controller-owned leaf is an error rather
// than a silent override: the engine keys are what make the mode work.
func mergeOverlayPatches(user, controller *task.ConfigPatchTask) (*task.ConfigPatchTask, error) {
	if controller == nil {
		return user, nil
	}
	if user == nil {
		return controller, nil
	}
	for file, table := range controller.Files {
		if user.Files[file] == nil {
			user.Files[file] = make(map[string]any)
		}
		if err := mergeTables(user.Files[file], table, file, ""); err != nil {
			return nil, err
		}
	}
	return user, nil
}

func mergeTables(dst, src map[string]any, file, prefix string) error {
	for key, value := range src {
		path := prefix + key
		incoming, isMap := value.(map[string]any)
		current, wasMap := dst[key].(map[string]any)
		if isMap && wasMap {
			if err := mergeTables(current, incoming, file, path+"."); err != nil {
				return err
			}
			continue
		}
		if _, taken := dst[key]; taken {
			return fmt.Errorf("configValues %s:%s: this key is set by spec.consensus and cannot be overridden", file, path)
		}
		if isMap {
			dst[key] = maps.Clone(incoming)
			continue
		}
		dst[key] = value
	}
	return nil
}
