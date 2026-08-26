package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// killTimeout is sized to agent drain; the cluster's max_kill_timeout is the
// cap (04 §4, e2a-kill-shutdown-timers).
const killTimeout = 30 * time.Second

// lostAfter bounds the disconnect.replace=false unknown-client window (04
// §4); after it expires the reschedule block below (zeroed) governs, so no
// replacement alloc is ever created (R1c-08 serial-dependency note).
const lostAfter = 5 * time.Minute

// sessionCPU/sessionMemoryMB are the explicit per-session resource class (04
// §4 resources bullet). Sized for the fake/offline lane only — a real
// deployment tunes these per session class.
const (
	sessionCPU      = 200
	sessionMemoryMB = 256
)

// parentJobSpec builds the city's parameterized parent job — the job every
// session is dispatched from (04 §2 identity contract: session_name maps to
// a dispatch child of this job). It fences the Nomad-side reconciler out of
// session lifecycle per 04 §4 ("Nomad-side job template invariants"): GC is
// meant to be the ONLY self-healer for session jobs, so every knob that
// would let Nomad replace or restart a dispatched child on its own is
// zeroed or disabled here.
//
// nodePool is left empty by default (Nomad's own "default" node pool);
// NRT-P2-05 drift row 3 found that without an explicit value here, every
// registered parent/dispatch job lands in the default pool and never places
// on a lab cluster's named pool (e.g. lab-session) — so a deployment that
// needs non-default placement must set GC_NOMAD_NODE_POOL.
func parentJobSpec(namespace, nodePool, parentID string) nomadJob {
	job := nomadJob{
		ID:        parentID,
		Namespace: namespace,
		NodePool:  nodePool,
		Type:      "batch",
		// Priority stays below every cluster system job by the invariant's
		// margin (04 §4 priority/preemption invariant); the lab/offline
		// cluster this pack targets keeps service/batch preemption off, so
		// this default is a documented starting point, not an enforcement
		// mechanism on its own.
		Priority: 40,
		Constraints: []nomadConstraint{
			// host-network is a FATAL rule for session jobs (04 §4,
			// R2a-05): bridge/cni networking is Linux-only, so a
			// non-Linux placement must fail closed rather than silently
			// fall back to host mode.
			{LTarget: "${attr.kernel.name}", RTarget: "linux", Operand: "="},
		},
		ParameterizedJob: &nomadParameterizedJob{
			// Dispatch Meta carries only non-secret attribution
			// (gc_session + a per-dispatch nonce, 04 §2.1 rule); the
			// instance token/capability NEVER rides Meta (R2a-02).
			MetaOptional: []string{"gc_session", "gc_nonce"},
			Payload:      "forbidden",
		},
		TaskGroups: []nomadTaskGroup{sessionTaskGroup()},
	}
	// Stamp the jobspec's own fingerprint into its Meta (fnrt-t4l.9): a
	// registered parent that still exists and still matches on NodePool can
	// nonetheless carry a stale task (04 §3, e.g. t4l.7's supervisor-script
	// swap) that a NodePool-only comparison can never see. Computed over job
	// BEFORE this field is set, so the hash never includes itself.
	job.Meta = map[string]string{jobspecHashMetaKey: jobspecHash(job)}
	return job
}

// jobspecHashMetaKey is the parent job Meta key ensureParentRegistered reads
// back via getJob to detect drift beyond NodePool (fnrt-t4l.9).
const jobspecHashMetaKey = "gc_jobspec_hash"

// jobspecHash returns a short, stable fingerprint of job's registerable
// content. It must be called with job.Meta unset (parentJobSpec's only
// caller) so the hash never depends on the very field it gets stamped into.
func jobspecHash(job nomadJob) string {
	b, err := json.Marshal(job)
	if err != nil {
		// job is built entirely from static Go types (no funcs/chans), so
		// Marshal cannot fail here; a panic would only ever fire from a
		// future edit that adds an unmarshalable field.
		panic(fmt.Sprintf("jobspecHash: marshal parent job spec: %v", err))
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}

func sessionTaskGroup() nomadTaskGroup {
	return nomadTaskGroup{
		Name:  "session",
		Count: 1,
		// GC is the only self-healer for session jobs (04 §4): a dead
		// agent must produce a terminal alloc that STAYS terminal, so both
		// in-place restart and reschedule are disabled here.
		RestartPolicy: nomadRestartPolicy{Attempts: 0, Mode: "fail"},
		ReschedulePolicy: nomadReschedulePolicy{
			Attempts:  0,
			Unlimited: false,
		},
		// disconnect.replace=false governs only the unknown-client window;
		// after lost_after the zeroed reschedule block above is what
		// actually forbids a replacement alloc (04 §4 serial-dependency
		// note, R1c-08).
		Disconnect: &nomadDisconnect{
			Replace:   boolPtr(false),
			Reconcile: "keep_original",
			LostAfter: lostAfter.Nanoseconds(),
		},
		// bridge networking so the host-network fatal-rule constraint above
		// has a placement to fail closed against (04 §4, R2a-05).
		Networks: []nomadNetwork{{Mode: "bridge"}},
		Tasks:    []nomadTask{sessionTask()},
	}
}

// sessionSupervisorScript is the session task's own long-lived command (04
// §3 provision row + NRT-P2-06.1): a trap-and-loop that does nothing but
// stay alive until it is signaled, so the task — and therefore the alloc —
// stays running for the box's whole lifetime instead of exiting the instant
// Config.command returns. The placeholder this replaces (`/bin/true`)
// exited immediately on a real client, driving the alloc terminal before
// launch's alloc-exec call could ever reach it
// (ops/receipts/nrt-p2-06-1-density.md). It deliberately never touches tmux
// itself: buildLaunchCommand (ops.go) is what creates the tmuxSessionName
// session over alloc-exec, and a supervisor that pre-created that same
// session would collide with it ("duplicate session"). `wait "$!"` on a
// backgrounded sleep (rather than a foreground `sleep`) is what lets the
// TERM trap fire immediately instead of only between sleep ticks, so stop
// exits this loop with code 0 promptly — well inside kill_timeout — rather
// than needing Nomad's SIGKILL fallback once kill_timeout elapses.
const sessionSupervisorScript = `trap 'exit 0' TERM; while :; do sleep 5 & wait "$!"; done`

func sessionTask() nomadTask {
	return nomadTask{
		Name:   "agent",
		Driver: "exec",
		Config: map[string]any{
			"command": "/bin/sh",
			"args":    []string{"-c", sessionSupervisorScript},
		},
		Resources: nomadResources{
			CPU:      sessionCPU,
			MemoryMB: sessionMemoryMB,
		},
		KillTimeout: killTimeout.Nanoseconds(),
		// No identity{env|file}, Vault, Consul, or template blocks: Nomad's
		// default workload identity is fail-closed already, and this design
		// keeps it that way as a named control (04 §4, R2a-05) — so nothing
		// is declared here.
	}
}

func boolPtr(b bool) *bool { return &b }

// --- Nomad job-spec wire types (PascalCase JSON tags mirror the Nomad API,
// matching fakenomad's own field naming) ---

type nomadJob struct {
	ID               string                 `json:"ID"`
	Namespace        string                 `json:"Namespace"`
	NodePool         string                 `json:"NodePool,omitempty"`
	Type             string                 `json:"Type"`
	Priority         int                    `json:"Priority"`
	Constraints      []nomadConstraint      `json:"Constraints,omitempty"`
	ParameterizedJob *nomadParameterizedJob `json:"ParameterizedJob,omitempty"`
	TaskGroups       []nomadTaskGroup       `json:"TaskGroups"`
	// Meta carries jobspecHashMetaKey (fnrt-t4l.9) — the parent job's own
	// drift fingerprint, set by parentJobSpec and read back by
	// ensureParentRegistered via getJob. Unlike ParameterizedJob.MetaOptional
	// (the dispatch-time Meta keys a child MAY carry), this Meta rides the
	// parent job itself and is fixed at register time.
	Meta map[string]string `json:"Meta,omitempty"`
}

type nomadConstraint struct {
	LTarget string `json:"LTarget"`
	RTarget string `json:"RTarget"`
	Operand string `json:"Operand"`
}

type nomadParameterizedJob struct {
	MetaOptional []string `json:"MetaOptional,omitempty"`
	Payload      string   `json:"Payload,omitempty"`
}

type nomadTaskGroup struct {
	Name             string                `json:"Name"`
	Count            int                   `json:"Count"`
	RestartPolicy    nomadRestartPolicy    `json:"RestartPolicy"`
	ReschedulePolicy nomadReschedulePolicy `json:"ReschedulePolicy"`
	Disconnect       *nomadDisconnect      `json:"Disconnect,omitempty"`
	Networks         []nomadNetwork        `json:"Networks,omitempty"`
	Tasks            []nomadTask           `json:"Tasks"`
}

type nomadRestartPolicy struct {
	Attempts int    `json:"Attempts"`
	Mode     string `json:"Mode"`
}

type nomadReschedulePolicy struct {
	Attempts  int  `json:"Attempts"`
	Unlimited bool `json:"Unlimited"`
}

type nomadDisconnect struct {
	Replace   *bool  `json:"Replace,omitempty"`
	Reconcile string `json:"Reconcile,omitempty"`
	// Nomad's API marshals time.Duration fields as int64 nanoseconds, not
	// a Go duration string (empirically confirmed against real Nomad
	// v2.0.5 — NRT-P2-05; fakenomad never validated this, so the drift
	// was invisible to the offline L1 suite until run against a real
	// cluster).
	LostAfter int64 `json:"LostAfter,omitempty"`
}

type nomadNetwork struct {
	Mode string `json:"Mode"`
}

type nomadTask struct {
	Name        string         `json:"Name"`
	Driver      string         `json:"Driver"`
	Config      map[string]any `json:"Config,omitempty"`
	Resources   nomadResources `json:"Resources"`
	KillTimeout int64          `json:"KillTimeout,omitempty"`
}

type nomadResources struct {
	CPU      int `json:"CPU"`
	MemoryMB int `json:"MemoryMB"`
}
