package nodegroup

import (
	"encoding/json"
	"fmt"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const defaultGenesisBucket = "sei-genesis-ceremony-artifacts"

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

// genesisS3Config returns the S3 destination for genesis artifacts, applying
// defaults when the user omits the field.
func genesisS3Config(group *seiv1alpha1.SeiNodeGroup) seiv1alpha1.GenesisS3Destination {
	gc := group.Spec.Genesis
	if gc.GenesisS3 != nil {
		dest := *gc.GenesisS3
		if dest.Prefix == "" {
			dest.Prefix = fmt.Sprintf("%s/%s/", gc.ChainID, group.Name)
		}
		return dest
	}
	return seiv1alpha1.GenesisS3Destination{
		Bucket: defaultGenesisBucket,
		Prefix: fmt.Sprintf("%s/%s/", gc.ChainID, group.Name),
		Region: "eu-central-1",
	}
}
