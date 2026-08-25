package reconcilersim

import "time"

// DeathClass is the verdict a drill assigns after killing something and
// re-observing: whether the alloc (the "box") itself went non-running, or
// only the agent process inside it died while the alloc stayed running.
// This is the 08 §1.1 "in-box agent kill" vs "box kill" distinction — the
// harness's job is telling the two apart from observations alone, the same
// way a reconciler would have to.
type DeathClass int

const (
	// DeathUnknown means the observation didn't move at all — neither the
	// alloc's ClientStatus nor the is-running answer changed. A drill
	// treats this as its own failure (the injected kill had no observed
	// effect), not a classification.
	DeathUnknown DeathClass = iota
	// DeathAgent is an in-box agent kill: the alloc's ClientStatus stayed
	// "running" (the box/tmux-server/task-main survived) while the RPP
	// is-running probe reports false — 04 §3's provision-row distinction
	// between "alloc alive" and "agent alive".
	DeathAgent
	// DeathBox is a box kill: the alloc's ClientStatus itself went
	// terminal (failed/lost/complete) — the whole task-main died, not
	// just the agent process inside it.
	DeathBox
)

func (d DeathClass) String() string {
	switch d {
	case DeathAgent:
		return "agent-death"
	case DeathBox:
		return "box-death"
	default:
		return "unknown"
	}
}

// runningAllocStatuses are Nomad ClientStatus values that count as the
// alloc/box still being alive, per 04 §4's fencing rules ("pending" covers
// the placement window before the first status lands).
var runningAllocStatuses = map[string]bool{
	"pending": true,
	"running": true,
}

// ClassifyDeath compares the alloc status observed right before a kill was
// injected (before) against the alloc status observed after (after), plus
// whether the RPP is-running probe answered true or false after the kill,
// and returns the death class. before/after are the SAME allocation's two
// observations (same ID) — a drill that swaps in a fresh AllocRecord after
// a scheduler replacement should classify that separately (see
// CountReplacementAllocs) rather than through this function.
func ClassifyDeath(before, after AllocRecord, isRunningAfter bool) DeathClass {
	boxAliveBefore := runningAllocStatuses[before.ClientStatus]
	boxAliveAfter := runningAllocStatuses[after.ClientStatus]

	if !boxAliveBefore {
		return DeathUnknown
	}
	if boxAliveAfter && !isRunningAfter {
		return DeathAgent
	}
	if !boxAliveAfter {
		return DeathBox
	}
	return DeathUnknown
}

// HonestyVerdict is the result of checking the 04 §6 observation-honesty
// split during a fault window: is-running/list-running must never flip to
// a false "not running"/empty answer on mere API unavailability — they
// answer last-known-good instead (NRT-OBS-001..004).
type HonestyVerdict struct {
	// Honest is true iff the observed answer matched the last-known-good
	// expectation rather than reporting a false negative.
	Honest bool
	// Detail explains a dishonest verdict (empty when Honest is true).
	Detail string
}

// CheckIsRunningHonesty checks a single is-running observation taken during
// a fault window against the last-known-good state. A propagated error
// (lookupErr != nil) is honest only if wasRunningBefore is false — an error
// on a session that WAS running must still answer true/last-known-good, not
// surface the error as a silent "not running".
func CheckIsRunningHonesty(wasRunningBefore bool, observedRunning bool, lookupErr error) HonestyVerdict {
	if lookupErr != nil {
		if wasRunningBefore {
			return HonestyVerdict{Honest: false, Detail: "is-running returned an error for a session last known running; want last-known-good true, not a surfaced error"}
		}
		return HonestyVerdict{Honest: true}
	}
	if wasRunningBefore && !observedRunning {
		return HonestyVerdict{Honest: false, Detail: "is-running flipped to false during a fault window for a session last known running; want last-known-good true"}
	}
	return HonestyVerdict{Honest: true}
}

// StalenessAge is how long it's been since the last confirmed-good
// observation of a session — the datum the L4 partition drills' "staleness
// alarm fires past bound" pass criterion needs a caller to threshold.
func StalenessAge(lastGoodObservation, now time.Time) time.Duration {
	if now.Before(lastGoodObservation) {
		return 0
	}
	return now.Sub(lastGoodObservation)
}

// CountReplacementAllocs returns how many allocations beyond the first ever
// existed for a job — the density/box-kill/client-agent-kill/drain drills'
// "zero replacement allocs" pass criterion (Nomad self-healing must stay
// fenced off, 04 §4) is CountReplacementAllocs(allocs) == 0. allocs should
// be every allocation ListAllocsForJob ever returned for the job, not just
// the current snapshot.
func CountReplacementAllocs(allocs []AllocRecord) int {
	if len(allocs) == 0 {
		return 0
	}
	return len(allocs) - 1
}

// EgressOrdering is the GC-vs-egress race drill's pass criterion: the
// stop-path transcript/evidence copy (04 §3 stop row) must complete before
// the job is confirmed deregistered — never the other way around, and never
// silently skipped (an evidence_lost tombstone marker is an accepted
// outcome; silent loss is not).
type EgressOrdering struct {
	EgressObservedAt     time.Time
	DeregisterObservedAt time.Time
	EgressCompleted      bool
	EvidenceLostMarked   bool
}

// Ordered reports whether the observed ordering satisfies the drill: either
// egress completed strictly before deregister was observed, or the
// evidence_lost marker was written (the priced, non-silent fallback path,
// R1c-05). Anything else — deregister observed with egress neither
// completed nor marked lost — is a silent-loss failure.
func (o EgressOrdering) Ordered() bool {
	if o.EvidenceLostMarked {
		return true
	}
	if !o.EgressCompleted {
		return false
	}
	return !o.EgressObservedAt.After(o.DeregisterObservedAt)
}
