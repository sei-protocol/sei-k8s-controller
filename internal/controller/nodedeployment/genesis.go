package nodegroup

import (
	"encoding/json"

	"sigs.k8s.io/controller-runtime/pkg/log"
)

func marshalOverrides(overrides map[string]string) string {
	if len(overrides) == 0 {
		return ""
	}
	data, err := json.Marshal(overrides)
	if err != nil {
		log.Log.Error(err, "failed to marshal genesis overrides")
		return ""
	}
	return string(data)
}
