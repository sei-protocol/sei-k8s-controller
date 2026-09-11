package seinetwork

import (
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const (
	// heightReadingMaxAge is how old a child's committedHeightTime may be and
	// still count. Three node status polls: one missed poll is noise, three
	// means the node has stopped answering.
	heightReadingMaxAge = 90 * time.Second

	// producingWindow is how long the observed height may sit unchanged
	// before the network is no longer Producing. It doubles as the grace
	// window between the first fresh reading and the first block.
	producingWindow = 2 * time.Minute
)

// setProducingCondition derives Producing from the children's committed
// heights and advances status.observedHeight. It never consults pod
// readiness: a chain whose pods are all Ready but which commits nothing is
// exactly the case this condition exists to name.
//
//	no children                       -> False / NoNodes
//	no child read within maxAge       -> False / SignalUnreadable
//	highest fresh height rose         -> True  / HeightAdvancing
//	unchanged, inside window, height 0-> False / AwaitingFirstBlock
//	unchanged, inside window          -> True  / HeightAdvancing
//	unchanged, past window, on-demand -> False / Idle
//	unchanged, past window            -> False / HeightStalled
//
// A fresh height below the recorded high-water mark (chain rebuilt from
// genesis) resets the mark rather than being reported as a stall forever.
func setProducingCondition(network *seiv1alpha1.SeiNetwork, nodes []seiv1alpha1.SeiNode, now time.Time) {
	if len(nodes) == 0 {
		setCondition(network, seiv1alpha1.ConditionProducing, metav1.ConditionFalse,
			seiv1alpha1.ReasonNoNodes, "no child SeiNodes exist")
		return
	}

	highest, fresh, stale := highestFreshHeight(nodes, now)
	if fresh == 0 {
		setCondition(network, seiv1alpha1.ConditionProducing, metav1.ConditionFalse,
			seiv1alpha1.ReasonSignalUnreadable,
			fmt.Sprintf("no committed-height reading newer than %s from any of %d child nodes (%d stale, %d never read)",
				heightReadingMaxAge, len(nodes), stale, len(nodes)-stale))
		return
	}

	prev := network.Status.ObservedHeight
	if prev == nil || highest != prev.Height {
		network.Status.ObservedHeight = &seiv1alpha1.ObservedHeight{Height: highest, Time: metav1.NewTime(now)}
	}
	advanced := (prev != nil && highest > prev.Height) || (prev == nil && highest > 0)
	if advanced {
		setCondition(network, seiv1alpha1.ConditionProducing, metav1.ConditionTrue,
			seiv1alpha1.ReasonHeightAdvancing,
			fmt.Sprintf("observed height advanced to %d (%d/%d child readings fresh)", highest, fresh, len(nodes)))
		return
	}

	sinceAdvance := now.Sub(network.Status.ObservedHeight.Time.Time).Truncate(time.Second)
	if sinceAdvance <= producingWindow {
		if highest == 0 {
			setCondition(network, seiv1alpha1.ConditionProducing, metav1.ConditionFalse,
				seiv1alpha1.ReasonAwaitingFirstBlock,
				fmt.Sprintf("no block committed yet; %s of %s grace window elapsed", sinceAdvance, producingWindow))
			return
		}
		setCondition(network, seiv1alpha1.ConditionProducing, metav1.ConditionTrue,
			seiv1alpha1.ReasonHeightAdvancing,
			fmt.Sprintf("observed height %d unchanged for %s (window %s)", highest, sinceAdvance, producingWindow))
		return
	}

	if network.Spec.Consensus.CommitsOnDemand() {
		setCondition(network, seiv1alpha1.ConditionProducing, metav1.ConditionFalse,
			seiv1alpha1.ReasonIdle,
			fmt.Sprintf("engine commits only when transactions arrive (allowEmptyBlocks off); height %d unchanged for %s", highest, sinceAdvance))
		return
	}
	setCondition(network, seiv1alpha1.ConditionProducing, metav1.ConditionFalse,
		seiv1alpha1.ReasonHeightStalled,
		fmt.Sprintf("observed height %d has not advanced for %s (window %s)", highest, sinceAdvance, producingWindow))
}

// highestFreshHeight returns the highest committedHeight among children whose
// reading is at most heightReadingMaxAge old, how many children contributed,
// and how many had a reading that was too old.
func highestFreshHeight(nodes []seiv1alpha1.SeiNode, now time.Time) (highest int64, fresh, stale int) {
	for i := range nodes {
		st := nodes[i].Status
		if st.CommittedHeight == nil || st.CommittedHeightTime == nil {
			continue
		}
		if now.Sub(st.CommittedHeightTime.Time) > heightReadingMaxAge {
			stale++
			continue
		}
		fresh++
		if *st.CommittedHeight > highest {
			highest = *st.CommittedHeight
		}
	}
	return highest, fresh, stale
}
