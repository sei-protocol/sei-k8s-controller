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
	// means the node has stopped answering. Must exceed the node controller's
	// poll interval plus its failed-read backoff (node.heightReadBackoff), or
	// a single failed read ages a live node's stamp out before it is re-read.
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
//	no children                         -> False / NoNodes
//	no child ever read                  -> False / AwaitingFirstBlock
//	no child read within maxAge         -> False / SignalUnreadable
//	highest fresh height rose           -> True  / HeightAdvancing
//	unchanged, inside window, height 0  -> False / AwaitingFirstBlock
//	unchanged, inside window            -> True  / HeightAdvancing
//	unchanged, past window, on-demand   -> False / Idle
//	unchanged, past window              -> False / HeightStalled
//
// A child whose height has never been read is one that has not reached
// Running (bootstrap, paused, a plan in flight) — not a fault. A child that
// once reported and then went quiet is; SignalUnreadable names that case
// only.
//
// The high-water mark moves down only when every child that has ever
// reported — stale readings included — sits below it: the chain was rebuilt
// from genesis under the same name. A drop in the fresh maximum alone is
// not evidence of that; it is what a leading child's reading aging out
// looks like, and resetting on it would let the leader's return score as an
// advance on a halted chain.
func setProducingCondition(network *seiv1alpha1.SeiNetwork, nodes []seiv1alpha1.SeiNode, now time.Time) {
	if len(nodes) == 0 {
		setCondition(network, seiv1alpha1.ConditionProducing, metav1.ConditionFalse,
			seiv1alpha1.ReasonNoNodes, "no child SeiNodes exist")
		return
	}

	r := readHeights(nodes, now)
	if r.fresh == 0 {
		if r.stale == 0 {
			setCondition(network, seiv1alpha1.ConditionProducing, metav1.ConditionFalse,
				seiv1alpha1.ReasonAwaitingFirstBlock,
				fmt.Sprintf("no committed-height reading yet from any of %d child nodes (none Running long enough to report)", len(nodes)))
			return
		}
		setCondition(network, seiv1alpha1.ConditionProducing, metav1.ConditionFalse,
			seiv1alpha1.ReasonSignalUnreadable,
			fmt.Sprintf("no committed-height reading newer than %s from any of %d child nodes (%d stale, %d never read)",
				heightReadingMaxAge, len(nodes), r.stale, len(nodes)-r.stale))
		return
	}

	prev := network.Status.ObservedHeight
	switch {
	case prev == nil, r.highestFresh > prev.Height, r.highestAny < prev.Height:
		network.Status.ObservedHeight = &seiv1alpha1.ObservedHeight{Height: r.highestFresh, Time: metav1.NewTime(now)}
	}
	mark := network.Status.ObservedHeight
	advanced := (prev != nil && r.highestFresh > prev.Height) || (prev == nil && r.highestFresh > 0)
	if advanced {
		setCondition(network, seiv1alpha1.ConditionProducing, metav1.ConditionTrue,
			seiv1alpha1.ReasonHeightAdvancing,
			fmt.Sprintf("observed height advanced to %d (%d/%d child readings fresh)", mark.Height, r.fresh, len(nodes)))
		return
	}

	sinceAdvance := now.Sub(mark.Time.Time).Truncate(time.Second)
	if sinceAdvance <= producingWindow {
		if mark.Height == 0 {
			setCondition(network, seiv1alpha1.ConditionProducing, metav1.ConditionFalse,
				seiv1alpha1.ReasonAwaitingFirstBlock,
				fmt.Sprintf("no block committed yet; %s of %s grace window elapsed", sinceAdvance, producingWindow))
			return
		}
		setCondition(network, seiv1alpha1.ConditionProducing, metav1.ConditionTrue,
			seiv1alpha1.ReasonHeightAdvancing,
			fmt.Sprintf("observed height %d unchanged for %s (window %s; highest fresh reading %d)",
				mark.Height, sinceAdvance, producingWindow, r.highestFresh))
		return
	}

	if network.Spec.Consensus.CommitsOnDemand() {
		setCondition(network, seiv1alpha1.ConditionProducing, metav1.ConditionFalse,
			seiv1alpha1.ReasonIdle,
			fmt.Sprintf("engine commits only when transactions arrive (allowEmptyBlocks off); height %d unchanged for %s", mark.Height, sinceAdvance))
		return
	}
	setCondition(network, seiv1alpha1.ConditionProducing, metav1.ConditionFalse,
		seiv1alpha1.ReasonHeightStalled,
		fmt.Sprintf("observed height %d has not advanced for %s (window %s; highest fresh reading %d)",
			mark.Height, sinceAdvance, producingWindow, r.highestFresh))
}

// heightReadings summarises the children's committedHeight stamps at one
// instant.
type heightReadings struct {
	// highestFresh is the highest height among readings at most
	// heightReadingMaxAge old.
	highestFresh int64
	// highestAny is the highest height among all readings, stale included.
	highestAny int64
	// fresh and stale count children with a reading inside and outside
	// heightReadingMaxAge; children never read count in neither.
	fresh, stale int
}

func readHeights(nodes []seiv1alpha1.SeiNode, now time.Time) heightReadings {
	var r heightReadings
	for i := range nodes {
		st := nodes[i].Status
		if st.CommittedHeight == nil || st.CommittedHeightTime == nil {
			continue
		}
		r.highestAny = max(r.highestAny, *st.CommittedHeight)
		if now.Sub(st.CommittedHeightTime.Time) > heightReadingMaxAge {
			r.stale++
			continue
		}
		r.fresh++
		r.highestFresh = max(r.highestFresh, *st.CommittedHeight)
	}
	return r
}
