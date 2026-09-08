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

