package sei

import (
	"fmt"
	"testing"
)

// witnessA and witnessB are the two distinct bare host:port light-client
// witnesses reused across the state-sync validation tests in this package.
// Named consts (mirroring sdk/sei/provider/k8s) keep the literal below the
// goconst threshold. The values are placeholders — the shape (host:port), not
// the address, is what these tests exercise.
const (
	witnessA = "a:26657"
	witnessB = "b:26657"
)

// appToml and rateLimitKey name the config value the ConfigValues validation
// cases set.
const (
	appToml      = "app.toml"
	rateLimitKey = "evm.rate_limiting_enabled"
)

func TestValidateNetworkSpec(t *testing.T) {
	good := NetworkSpec{Name: "net", Image: "img", Validators: 1}
	cases := []struct {
		name    string
		mut     func(*NetworkSpec)
		wantErr bool
	}{
		{"valid", func(*NetworkSpec) {}, false},
		{"missing name", func(s *NetworkSpec) { s.Name = "" }, true},
		{"missing image", func(s *NetworkSpec) { s.Image = "" }, true},
		{"validators zero", func(s *NetworkSpec) { s.Validators = 0 }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := good
			tc.mut(&s)
			if err := validateNetworkSpec(s); (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func TestValidateNodeSpec(t *testing.T) {
	good := NodeSpec{Name: "rpc-0", Network: "net", Image: "img"}
	cases := []struct {
		name    string
		mut     func(*NodeSpec)
		wantErr bool
	}{
		{"valid", func(*NodeSpec) {}, false},
		{"missing name", func(s *NodeSpec) { s.Name = "" }, true},
		{"missing network", func(s *NodeSpec) { s.Network = "" }, true},
		{"missing image", func(s *NodeSpec) { s.Image = "" }, true},
		{"statesync one witness", func(s *NodeSpec) { s.StateSync = &NodeStateSync{RpcServers: []string{witnessA}} }, true},
		{"statesync two identical witnesses", func(s *NodeSpec) { s.StateSync = &NodeStateSync{RpcServers: []string{witnessA, witnessA}} }, true},
		{"statesync two distinct witnesses", func(s *NodeSpec) { s.StateSync = &NodeStateSync{RpcServers: []string{witnessA, witnessB}} }, false},
		{"config value", func(s *NodeSpec) {
			s.ConfigValues = []ConfigValue{{FileName: appToml, Key: rateLimitKey, Value: false}}
		}, false},
		{"config value after valid statesync", func(s *NodeSpec) {
			s.StateSync = &NodeStateSync{RpcServers: []string{witnessA, witnessB}}
			s.ConfigValues = []ConfigValue{{FileName: appToml, Key: rateLimitKey}}
		}, true},
		{"config value missing file name", func(s *NodeSpec) { s.ConfigValues = []ConfigValue{{Key: rateLimitKey, Value: false}} }, true},
		{"config value missing key", func(s *NodeSpec) { s.ConfigValues = []ConfigValue{{FileName: appToml, Value: false}} }, true},
		{"config value nil", func(s *NodeSpec) { s.ConfigValues = []ConfigValue{{FileName: appToml, Key: rateLimitKey}} }, true},
		{"config value with no JSON form", func(s *NodeSpec) {
			s.ConfigValues = []ConfigValue{{FileName: appToml, Key: rateLimitKey, Value: make(chan int)}}
		}, true},
		{"config value typed nil pointer", func(s *NodeSpec) {
			s.ConfigValues = []ConfigValue{{FileName: appToml, Key: rateLimitKey, Value: (*bool)(nil)}}
		}, true},
		{"config value nil map", func(s *NodeSpec) {
			s.ConfigValues = []ConfigValue{{FileName: appToml, Key: rateLimitKey, Value: map[string]any(nil)}}
		}, true},
		{"config value file name without .toml", func(s *NodeSpec) {
			s.ConfigValues = []ConfigValue{{FileName: "app", Key: rateLimitKey, Value: false}}
		}, true},
		{"config value key with an empty segment", func(s *NodeSpec) {
			s.ConfigValues = []ConfigValue{{FileName: appToml, Key: "evm..rate_limiting_enabled", Value: false}}
		}, true},
		{"config value set twice", func(s *NodeSpec) {
			s.ConfigValues = []ConfigValue{
				{FileName: appToml, Key: rateLimitKey, Value: false},
				{FileName: appToml, Key: rateLimitKey, Value: true},
			}
		}, true},
		{"same key in two files", func(s *NodeSpec) {
			s.ConfigValues = []ConfigValue{
				{FileName: appToml, Key: rateLimitKey, Value: false},
				{FileName: "config.toml", Key: rateLimitKey, Value: false},
			}
		}, false},
		{"config values over the CRD limit", func(s *NodeSpec) {
			for i := range 101 {
				s.ConfigValues = append(s.ConfigValues, ConfigValue{FileName: appToml, Key: fmt.Sprintf("k%d", i), Value: i})
			}
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := good
			tc.mut(&s)
			if err := validateNodeSpec(s); (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
		})
	}
}
