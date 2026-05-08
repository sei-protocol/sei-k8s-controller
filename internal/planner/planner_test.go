package planner

import "testing"

func TestTaskMaxRetries(t *testing.T) {
	cases := map[string]int{
		TaskConfigureGenesis: genesisConfigureMaxRetries,
		TaskAssembleGenesis:  groupAssemblyMaxRetries,
		TaskDiscoverPeers:    discoverPeersMaxRetries,
		"unknown-task-type":  0,
		"":                   0,
	}
	for taskType, want := range cases {
		if got := taskMaxRetries(taskType); got != want {
			t.Errorf("taskMaxRetries(%q) = %d, want %d", taskType, got, want)
		}
	}
}
