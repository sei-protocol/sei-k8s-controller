package task

import sidecar "github.com/sei-protocol/seictl/sidecar/client"

// ConfigApplyParams are the serialized fields for config-apply.
type ConfigApplyParams struct {
	Mode          string            `json:"mode"`
	Overrides     map[string]string `json:"overrides,omitempty"`
	Incremental   bool              `json:"incremental,omitempty"`
	TargetVersion int               `json:"targetVersion,omitempty"`
}

func (p *ConfigApplyParams) taskType() string { return sidecar.TaskTypeConfigApply }

func (p *ConfigApplyParams) toRequestParams() *map[string]any {
	m := map[string]any{
		"mode":          p.Mode,
		"incremental":   p.Incremental,
		"targetVersion": p.TargetVersion,
	}
	if len(p.Overrides) > 0 {
		overrides := make(map[string]any, len(p.Overrides))
		for k, v := range p.Overrides {
			overrides[k] = v
		}
		m["overrides"] = overrides
	}
	return &m
}

// ConfigValidateParams are the serialized fields for config-validate.
// This task takes no parameters.
type ConfigValidateParams struct{}

func (p *ConfigValidateParams) taskType() string { return sidecar.TaskTypeConfigValidate }

func (p *ConfigValidateParams) toRequestParams() *map[string]any { return nil }

// ConfigureGenesisParams are the serialized fields for configure-genesis.
// The sidecar resolves genesis from the chain ID autonomously — no params needed.
type ConfigureGenesisParams struct{}

func (p *ConfigureGenesisParams) taskType() string { return sidecar.TaskTypeConfigureGenesis }

func (p *ConfigureGenesisParams) toRequestParams() *map[string]any { return nil }

// DiscoverPeersParams are the serialized fields for discover-peers.
type DiscoverPeersParams struct {
	Sources []PeerSourceParam `json:"sources"`
}

// PeerSourceParam is the serialized form of a single peer source.
type PeerSourceParam struct {
	Type      string            `json:"type"`
	Region    string            `json:"region,omitempty"`
	Tags      map[string]string `json:"tags,omitempty"`
	Addresses []string          `json:"addresses,omitempty"`
}

func (p *DiscoverPeersParams) taskType() string { return sidecar.TaskTypeDiscoverPeers }

func (p *DiscoverPeersParams) toRequestParams() *map[string]any {
	sources := make([]any, len(p.Sources))
	for i, src := range p.Sources {
		m := map[string]any{"type": src.Type}
		switch sidecar.PeerSourceType(src.Type) {
		case sidecar.PeerSourceEC2Tags:
			m["region"] = src.Region
			tags := make(map[string]any, len(src.Tags))
			for k, v := range src.Tags {
				tags[k] = v
			}
			m["tags"] = tags
		case sidecar.PeerSourceStatic:
			addrs := make([]any, len(src.Addresses))
			for j, a := range src.Addresses {
				addrs[j] = a
			}
			m["addresses"] = addrs
		}
		sources[i] = m
	}
	m := map[string]any{"sources": sources}
	return &m
}

// MarkReadyParams are the serialized fields for mark-ready.
// This task takes no parameters.
type MarkReadyParams struct{}

func (p *MarkReadyParams) taskType() string { return sidecar.TaskTypeMarkReady }

func (p *MarkReadyParams) toRequestParams() *map[string]any { return nil }
