package sei

import "testing"

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
		{"statesync one witness", func(s *NodeSpec) { s.StateSync = &NodeStateSync{RpcServers: []string{"a:26657"}} }, true},
		{"statesync two witnesses", func(s *NodeSpec) { s.StateSync = &NodeStateSync{RpcServers: []string{"a:26657", "b:26657"}} }, false},
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
