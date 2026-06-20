package sei

import "time"

// NetworkSpec is the typed input to ProvisionNetwork — the union of the WS-A
// `seictl network apply` flag surface. Only fields the shipped CLI carries are
// exposed; Overrides is the escape hatch so the typed surface need not chase
// every preset knob (LLD §3.3).
//
// Strategic-merge Set (seictl `--set` parity) is deferred until a consumer needs
// it — cleanly merging arbitrary paths into the typed CRs is out of MVP scope.
// Overrides is the supported config path.
type NetworkSpec struct {
	Name      string // metadata.name; also the follower peer-selector value (§5.3)
	Namespace string // "" => provider default (kubeconfig context / SA namespace)
	Preset    string // "genesis-chain" (the only WS-A network preset today)
	ChainID   string // -> spec.genesis.chainId (not a top-level field)
	Image     string // -> spec.image
	Replicas  int    // validator count; immutable post-create

	GenesisAccounts []GenesisAccount
	Overrides       map[string]string // -> spec.genesis.overrides (TOML-path keys)

	ReadyTimeout time.Duration // default 15m, mirrors seictl network watch
}

// GenesisAccount is a non-validator genesis account to fund.
type GenesisAccount struct {
	Address string
	Balance string
}

// FleetSpec is the typed input to ProvisionFleet — N follower SeiNodes peered
// to the Network. Network peering is derived from the *Network arg, never set
// here: the SDK owns the sei.io/seinetwork peer-wiring + object-label contract
// (LLD §3.3/§5.3).
type FleetSpec struct {
	NamePrefix string // nodes are <NamePrefix>-0..N-1
	Namespace  string // "" => same as Network
	Preset     string // "rpc" (the only WS-A node preset today)
	Replicas   int    // N; >= 1
	Image      string
	Overrides  map[string]string // -> spec.overrides; supported config path (Set deferred, see NetworkSpec)

	RunningTimeout    time.Duration // default 15m — wait all N -> PhaseRunning
	FirstBlockTimeout time.Duration // default 5m  — per-node readiness gate (§5.5)
	PollInterval      time.Duration // default 5s
}

// Default timeouts mirror seictl/seitask (provisionnode withDefaults).
const (
	defaultReadyTimeout      = 15 * time.Minute
	defaultRunningTimeout    = 15 * time.Minute
	defaultFirstBlockTimeout = 5 * time.Minute
	defaultPollInterval      = 5 * time.Second
)

// withDefaults returns a copy of spec with zero-valued timeouts filled.
func (s NetworkSpec) withDefaults() NetworkSpec {
	if s.ReadyTimeout == 0 {
		s.ReadyTimeout = defaultReadyTimeout
	}
	return s
}

func (s FleetSpec) withDefaults() FleetSpec {
	if s.RunningTimeout == 0 {
		s.RunningTimeout = defaultRunningTimeout
	}
	if s.FirstBlockTimeout == 0 {
		s.FirstBlockTimeout = defaultFirstBlockTimeout
	}
	if s.PollInterval == 0 {
		s.PollInterval = defaultPollInterval
	}
	return s
}
