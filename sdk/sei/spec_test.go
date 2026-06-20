package sei

import "testing"

func TestValidateNetworkSpec(t *testing.T) {
	good := NetworkSpec{Name: "net", ChainID: "sei-1", Image: "img", Replicas: 1}
	cases := []struct {
		name    string
		mut     func(*NetworkSpec)
		wantErr bool
	}{
		{"valid", func(*NetworkSpec) {}, false},
		{"missing name", func(s *NetworkSpec) { s.Name = "" }, true},
		{"missing chainID", func(s *NetworkSpec) { s.ChainID = "" }, true},
		{"missing image", func(s *NetworkSpec) { s.Image = "" }, true},
		{"replicas zero", func(s *NetworkSpec) { s.Replicas = 0 }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := good
			tc.mut(&s)
			err := validateNetworkSpec(s)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if tc.wantErr && !IsUsage(t, err) {
				t.Fatalf("spec error should be ClassUsage, got %v", err)
			}
		})
	}
}

func TestValidateFleetSpec(t *testing.T) {
	good := FleetSpec{NamePrefix: "rpc", Image: "img", Replicas: 1}
	cases := []struct {
		name    string
		mut     func(*FleetSpec)
		wantErr bool
	}{
		{"valid", func(*FleetSpec) {}, false},
		{"missing prefix", func(s *FleetSpec) { s.NamePrefix = "" }, true},
		{"missing image", func(s *FleetSpec) { s.Image = "" }, true},
		{"replicas zero", func(s *FleetSpec) { s.Replicas = 0 }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := good
			tc.mut(&s)
			err := validateFleetSpec(s)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func TestSpecDefaults(t *testing.T) {
	n := NetworkSpec{}.withDefaults()
	if n.ReadyTimeout != defaultReadyTimeout {
		t.Errorf("ReadyTimeout = %v, want %v", n.ReadyTimeout, defaultReadyTimeout)
	}
	f := FleetSpec{}.withDefaults()
	if f.RunningTimeout != defaultRunningTimeout || f.FirstBlockTimeout != defaultFirstBlockTimeout || f.PollInterval != defaultPollInterval {
		t.Errorf("fleet defaults not applied: %+v", f)
	}
	// A caller-set value is preserved.
	if got := (FleetSpec{PollInterval: 1}).withDefaults().PollInterval; got != 1 {
		t.Errorf("caller PollInterval overwritten: %v", got)
	}
}

func TestEVMRPCList_SkipsEmpty(t *testing.T) {
	fe := FleetEndpoints{Nodes: []FleetNodeEndpoint{
		{Name: "rpc-0", EvmJsonRPC: rpc0URL},
		{Name: "rpc-1"}, // no EVM URL — skipped
		{Name: "rpc-2", EvmJsonRPC: rpc2URL},
	}}
	got := fe.EVMRPCList()
	want := []string{rpc0URL, rpc2URL}
	if len(got) != len(want) {
		t.Fatalf("EVMRPCList = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("EVMRPCList[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
