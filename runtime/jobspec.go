package main

import "time"

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
func parentJobSpec(namespace, parentID string) nomadJob {
	return nomadJob{
		ID:        parentID,
		Namespace: namespace,
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
			LostAfter: lostAfter.String(),
		},
		// bridge networking so the host-network fatal-rule constraint above
		// has a placement to fail closed against (04 §4, R2a-05).
		Networks: []nomadNetwork{{Mode: "bridge"}},
		Tasks:    []nomadTask{sessionTask()},
	}
}

func sessionTask() nomadTask {
	return nomadTask{
		Name: "agent",
		// Real driver/config (the tmux+agent supervisor) is provision/
		// staging work — out of scope for this bead (NRT-P1-03 out_of_scope:
		// "driving verbs; staging; provision split"). This placeholder
		// keeps the job spec structurally valid for dispatch.
		Driver: "exec",
		Config: map[string]any{"command": "/bin/true"},
		Resources: nomadResources{
			CPU:      sessionCPU,
			MemoryMB: sessionMemoryMB,
		},
		KillTimeout: killTimeout.String(),
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
	Type             string                 `json:"Type"`
	Priority         int                    `json:"Priority"`
	Constraints      []nomadConstraint      `json:"Constraints,omitempty"`
	ParameterizedJob *nomadParameterizedJob `json:"ParameterizedJob,omitempty"`
	TaskGroups       []nomadTaskGroup       `json:"TaskGroups"`
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
	LostAfter string `json:"LostAfter,omitempty"`
}

type nomadNetwork struct {
	Mode string `json:"Mode"`
}

type nomadTask struct {
	Name        string         `json:"Name"`
	Driver      string         `json:"Driver"`
	Config      map[string]any `json:"Config,omitempty"`
	Resources   nomadResources `json:"Resources"`
	KillTimeout string         `json:"KillTimeout,omitempty"`
}

type nomadResources struct {
	CPU      int `json:"CPU"`
	MemoryMB int `json:"MemoryMB"`
}
