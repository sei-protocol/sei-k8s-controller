# Feature specifications

This directory holds Spec Kit feature specifications for the sei-k8s-controller.
Each spec sits in its own `NNN-slug/spec.md` directory and follows the template
from `sei-protocol/sei-internal-skills` (`writing/templates/spec-template.md`):
Semantic Anchors, Glossary, Boundary Context, EARS acceptance criteria, and
named verifiers. The writing contract's `writing/scripts/check-verifiers.sh`
gate checks that every success criterion names a verifier.

| Spec | Title | Status |
|---|---|---|
| [001](001-configurable-node-resources/spec.md) | Selectable node resources for benchmarks | Draft |
| [002](002-config-override-substrate/spec.md) | Arbitrary config override substrate | Draft |
| [003](003-config-substrate-parity-seinetwork/spec.md) | Config-value parity on the validator network | Draft |
| [004](004-crd-ownership-and-deletion/spec.md) | Predictable ownership and deletion | Draft |
| [005](005-ephemeral-teardown-and-prune/spec.md) | Ephemeral teardown and prune | Draft |
| [006](006-node-ec2-locality/spec.md) | One validator per EC2 for a benchmark | Draft |
