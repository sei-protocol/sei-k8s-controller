package nodegroup

import (
	"encoding/json"
)

func marshalOverrides(overrides map[string]string) string {
	if len(overrides) == 0 {
		return ""
	}
	data, err := json.Marshal(overrides)
	if err != nil {
		return ""
	}
	return string(data)
}
