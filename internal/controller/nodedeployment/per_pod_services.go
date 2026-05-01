package nodedeployment

import (
	"cmp"
	"slices"
	"strconv"

	"github.com/go-logr/logr"
	seiconfig "github.com/sei-protocol/sei-config"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// populatePerPodServices builds the PerPodServices status entries from the
// child SeiNode list. Pure function — no client calls. The headless Service
// for each child has the same name as the SeiNode (see
// noderesource.GenerateHeadlessService); ports come from seiconfig so a port
// change in the chain runtime propagates through one canonical source.
//
// Exporter-role children are excluded to mirror populateIncumbentNodes.
// Children missing the ordinal label are skipped (logged at V(1)) rather than
// emitted with ordinal=0, which would scramble the sort order.
//
// Returns nil when no usable children exist; the caller assigns directly to
// status without an empty-list intermediate so omitempty drops the field.
func populatePerPodServices(log logr.Logger, nodes []seiv1alpha1.SeiNode) []seiv1alpha1.PerPodServiceStatus {
	type entry struct {
		status  seiv1alpha1.PerPodServiceStatus
		ordinal int
	}
	entries := make([]entry, 0, len(nodes))
	for i := range nodes {
		node := &nodes[i]
		if node.Labels["sei.io/role"] == "exporter" {
			continue
		}
		ordStr, ok := node.Labels[groupOrdinalLabel]
		if !ok {
			log.V(1).Info("per-pod service: skipping node missing ordinal label", "node", node.Name)
			continue
		}
		ord, err := strconv.Atoi(ordStr)
		if err != nil {
			log.V(1).Info("per-pod service: skipping node with unparseable ordinal label",
				"node", node.Name, "label", ordStr)
			continue
		}
		entries = append(entries, entry{
			status: seiv1alpha1.PerPodServiceStatus{
				Name:      node.Name,
				Namespace: node.Namespace,
				Ports: seiv1alpha1.PerPodServicePorts{
					EvmHttp: seiconfig.PortEVMHTTP,
					EvmWs:   seiconfig.PortEVMWS,
				},
			},
			ordinal: ord,
		})
	}
	if len(entries) == 0 {
		return nil
	}
	slices.SortFunc(entries, func(a, b entry) int { return cmp.Compare(a.ordinal, b.ordinal) })
	out := make([]seiv1alpha1.PerPodServiceStatus, len(entries))
	for i := range entries {
		out[i] = entries[i].status
	}
	return out
}
