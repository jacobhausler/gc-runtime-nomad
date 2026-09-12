package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"
)

// logShipperConfig configures the optional log-shipper task (owner
// requirement: "first-class, one env var to enable").
// The zero value (Sink == "") disables the task entirely — sessionTaskGroup
// then builds the exact single-task group it always has, so an unset
// GC_NOMAD_LOG_SINK leaves every existing deployment's jobspec byte-for-byte
// unchanged.
type logShipperConfig struct {
	// Sink is the HTTP JSON-lines endpoint vector POSTs shipped log lines
	// to (GC_NOMAD_LOG_SINK). The one env var that turns this feature on.
	Sink string
	// TokenFile is an in-box path the log-shipper task reads its
	// Authorization bearer token's VALUE from at task start
	// (GC_NOMAD_LOG_SINK_TOKEN_FILE) — the token itself never rides this
	// job spec (mirrors the "no capability in Meta" rule for the
	// dispatch-identity token: this job spec is registered once and is
	// readable by anyone who can `nomad job inspect` the parent). Empty
	// disables the sink's auth block — an unauthenticated sink.
	TokenFile string
	// Labels is a raw "k=v,k=v" string (GC_NOMAD_LOG_LABELS) merged onto
	// every shipped log line alongside the fixed session_name/alloc_id/
	// node/runtime=nomad set (vectorConfigTOML's "label" transform).
	Labels string
	// Artifact is the URL or local path for the pinned Vector archive
	// (GC_NOMAD_LOG_SHIPPER_ARTIFACT). Empty keeps the upstream release URL
	// for deployments that have network access; offline deployments should
	// point this at a pre-staged archive on the Nomad client.
	Artifact string
}

// enabled reports whether the log-shipper task should be added to the
// session task group at all.
func (c logShipperConfig) enabled() bool { return c.Sink != "" }

// killTimeout is sized to agent drain; the cluster's max_kill_timeout is the
// cap.
const killTimeout = 30 * time.Second

// lostAfter bounds the disconnect.replace=false unknown-client window;
// after it expires the reschedule block below (zeroed) governs, so no
// replacement alloc is ever created (the serial-dependency note).
const lostAfter = 5 * time.Minute

// sessionCPU/sessionMemoryMB are the explicit per-session resource class.
// sessionSecretsMB overrides Nomad's 1 MiB secrets-tmpfs
// default: deployment bootstraps keep CODEX_HOME there so auth remains outside
// alloc-fs, and Codex's state database alone can exceed 300 MiB. Nomad counts
// this reservation in scheduling, unlike its unrequested 1 MiB default.
const (
	sessionCPU       = 200
	sessionMemoryMB  = 512
	sessionSecretsMB = 512
)

// parentJobSpec builds the city's parameterized parent job — the job every
// session is dispatched from (the identity contract: session_name maps to
// a dispatch child of this job). It fences the Nomad-side reconciler out of
// session lifecycle per the "Nomad-side job template invariants": GC is
// meant to be the ONLY self-healer for session jobs, so every knob that
// would let Nomad replace or restart a dispatched child on its own is
// zeroed or disabled here.
//
// nodePool is left empty by default (Nomad's own "default" node pool);
// A drift check found that without an explicit value here, every
// registered parent/dispatch job lands in the default pool and never places
// on a lab cluster's named pool (e.g. lab-session) — so a deployment that
// needs non-default placement must set GC_NOMAD_NODE_POOL.
//
// agentImage names the prebaked Docker image that owns the agent toolchain,
// and it is REQUIRED: the Docker task is the only session task shape
// (the legacy exec task has been deleted, and with it the
// GC_NOMAD_ARTIFACT_VOLUME host volume that used to deliver a Linux gc
// binary to it). With no second shape left to fall back to, newLifecycle
// refuses to build a lifecycle at all without an image rather than
// register a parent whose boxes can never run an agent.
func parentJobSpec(namespace, nodePool, parentID string, logShipper logShipperConfig, agentImage string, dep placementConfig) nomadJob {
	job := nomadJob{
		ID:        parentID,
		Namespace: namespace,
		NodePool:  nodePool,
		Type:      "batch",
		// Priority stays below every cluster system job by the invariant's
		// margin (the priority/preemption invariant); the lab/offline
		// cluster this pack targets keeps service/batch preemption off, so
		// this default is a documented starting point, not an enforcement
		// mechanism on its own.
		Priority: 40,
		Constraints: []nomadConstraint{
			// host-network is a FATAL rule for session jobs:
			// bridge/cni networking is Linux-only, so a
			// non-Linux placement must fail closed rather than silently
			// fall back to host mode.
			{LTarget: "${attr.kernel.name}", RTarget: "linux", Operand: "="},
			// amd64 is the second half of the same fatal rule. Everything
			// this pack can run is amd64 by construction:
			// toolchain/Dockerfile fetches the linux_amd64 gc release and
			// reads the binary-amd64 apt index, toolchain/build-image.sh
			// refuses to pin a gc that is not an x86-64 ELF, and the
			// log-shipper's vector artifact is pinned by URL AND sha256 to
			// x86_64-unknown-linux-gnu. Verified on a live client
			// 2026-09-12: `nomad node status -self -verbose` reports
			// `cpu.arch = amd64`, so this is the attribute and spelling
			// Nomad actually fingerprints.
			//
			// Without it an arm64 client places the group and only HALF of
			// it breaks. The agent is the group leader and the shipper is a
			// non-leader poststart sidecar, so a shipper that cannot exec
			// its wrong-arch binary tears nothing down and raises nothing —
			// the session runs, looks healthy, and its logs silently never
			// ship. Fail the placement closed instead, exactly as the
			// kernel rule above does for a non-Linux client.
			{LTarget: "${attr.cpu.arch}", RTarget: "amd64", Operand: "="},
		},
		ParameterizedJob: &nomadParameterizedJob{
			// Dispatch Meta carries only non-secret attribution
			// (gc_session + a per-dispatch nonce); the
			// instance token/capability NEVER rides Meta.
			// gc_created_at (client.go createdAtMetaKey) is the durable creation
			// stamp the binding-adoption gate reads back: Nomad's jobs list carries
			// SubmitTime, but a RECOVERED binding had no age, so adoption refused it
			// permanently. It was added as a dispatch key without being declared
			// here, and Nomad rejects an undeclared key outright - "Dispatch request
			// included unpermitted metadata keys: [gc_created_at]", HTTP 500, on
			// EVERY provision. Measured 2026-09-11 in the executive proof city: zero
			// boxes started on the 0a35596 pack until this list matched what the
			// dispatcher actually sends. Still non-secret attribution only - the
			// instance token/capability NEVER rides Meta.
			MetaOptional: []string{"gc_session", "gc_nonce", "gc_created_at", agentImageMetaKey},
			Payload:      "forbidden",
		},
		TaskGroups: []nomadTaskGroup{sessionTaskGroup(logShipper, agentImage, dep)},
	}
	// Stamp the jobspec's own fingerprint into its Meta: a
	// registered parent that still exists and still matches on NodePool can
	// nonetheless carry a stale task (e.g. a supervisor-script swap) that a
	// NodePool-only comparison can never see. Computed over job
	// BEFORE this field is set, so the hash never includes itself.
	// Deployment-configured node constraints ride AFTER the linux fatal rule, so
	// an operator placement fence can never displace it (jobspec_test guards the
	// ordering). Appended before the hash is stamped, so a placement change is
	// drift-visible to ensureParentRegistered rather than silently ignored on an
	// already-registered parent.
	job.Constraints = append(job.Constraints, dep.nodeConstraints...)
	// The deployment default rides in the parent's Meta and is hashed WITH the
	// job, so changing the default is drift-visible exactly as changing the
	// baked image used to be. A per-dispatch override never touches the parent,
	// which is the whole point: one parent, many images.
	job.Meta = map[string]string{agentImageMetaKey: agentImage}
	job.Meta[jobspecHashMetaKey] = jobspecHash(job)
	return job
}

// agentImageMetaKey is the dispatch metadata key carrying the agent image.
// The parent declares the deployment default under it; a dispatch may override
// it for one session.
const agentImageMetaKey = "gc_image"

// jobspecHashMetaKey is the parent job Meta key ensureParentRegistered reads
// back via getJob to detect drift beyond NodePool.
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

// placementConfig carries the deployment-configured placement input that rides
// the parent job: the resolvers the bridge network writes into the alloc's
// /etc/resolv.conf, and any node constraints the estate fences sessions to.
// Both are EMPTY by default and an empty value renders the jobspec byte-for-byte
// as it was before placement config existed — including its jobspecHash — so an unset
// deployment sees no drift and no behavior change. That is also the rollback:
// unset the variable and the pack goes back to the spec it shipped.
type placementConfig struct {
	dnsServers      []string
	nodeConstraints []nomadConstraint
}

// parseDNSServers reads GC_NOMAD_DNS_SERVERS: a comma-separated list of
// resolver IP addresses for the session group's bridge network. An address (not
// a hostname) is what Nomad's dns.servers takes, and a typo there reproduces
// this bug in a form nobody sees — the box starts fine and just cannot resolve
// anything — so a non-IP item is a config error that fails the op closed rather
// than a value logged and dropped.
func parseDNSServers(raw string) ([]string, error) {
	var servers []string
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if net.ParseIP(part) == nil {
			return nil, fmt.Errorf("parsing %s: %q is not an IP address (the value is a comma-separated list of resolver addresses such as 192.168.0.202)", envDNSServers, part)
		}
		servers = append(servers, part)
	}
	return servers, nil
}

// parseNodeConstraints reads GC_NOMAD_NODE_CONSTRAINT: node constraints to
// append to the parent job, written in Nomad's own syntax, e.g.
// "${node.unique.name} != prod-nomad". Several are separated by ';' rather than
// ',' because Nomad's list operands (in/not) already use commas inside the
// right-hand side. An unparsable entry is a config error: a silently dropped
// constraint would let sessions place where the estate said they may not.
func parseNodeConstraints(raw string) ([]nomadConstraint, error) {
	var constraints []nomadConstraint
	for _, part := range strings.Split(raw, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		fields := strings.Fields(part)
		if len(fields) != 3 {
			return nil, fmt.Errorf("parsing %s: %q is not \"<ltarget> <operand> <rtarget>\" (e.g. \"${node.unique.name} != prod-nomad\")", envNodeConstraint, part)
		}
		operand := fields[1]
		if !nomadConstraintOperands[operand] {
			return nil, fmt.Errorf("parsing %s: %q in %q is not an operand Nomad accepts (want one of = == != =~ !~ > < >= <= in not is bitset)", envNodeConstraint, operand, part)
		}
		constraints = append(constraints, nomadConstraint{LTarget: fields[0], RTarget: fields[2], Operand: operand})
	}
	return constraints, nil
}

// nomadConstraintOperands is the operand set Nomad's constraint block accepts;
// anything else is a typo that Nomad would reject at register time anyway, and
// catching it here names the offending variable instead of failing a start.
var nomadConstraintOperands = map[string]bool{
	"=": true, "==": true, "!=": true, "=~": true, "!~": true,
	">": true, "<": true, ">=": true, "<=": true,
	"in": true, "not": true, "is": true, "bitset": true,
}

func sessionTaskGroup(logShipper logShipperConfig, agentImage string, dep placementConfig) nomadTaskGroup {
	agent := sessionTask(agentImage)
	tasks := []nomadTask{agent}
	// bridge networking so the host-network fatal-rule constraint above
	// has a placement to fail closed against.
	networks := []nomadNetwork{{Mode: "bridge"}}
	if len(dep.dnsServers) > 0 {
		// A bridge alloc gets a resolv.conf written by Nomad from this stanza,
		// not one copied from the client host.
		networks[0].DNS = &nomadDNS{Servers: dep.dnsServers}
	}

	if logShipper.enabled() {
		// Leader:true makes Nomad stop the OTHER tasks in this group only
		// once the agent task itself exits (real Nomad group-leader
		// semantics) — the "kill_timeout ordering so the shipper outlives
		// the agent" scope line. logShipperTask's own KillTimeout (longer
		// than the agent's) then bounds just the shipper's own flush
		// window, not the agent's drain too.
		agent.Leader = true
		tasks[0] = agent
		tasks = append(tasks, logShipperTask(logShipper))
		// The one port this group ever declares: vector's built-in
		// prometheus_exporter, on a group-local dynamic port every task
		// sees as $NOMAD_PORT_metrics. Only declared
		// when the shipper task exists to bind it.
		networks[0].DynamicPorts = []nomadPort{{Label: logShipperMetricsPortLabel}}
	}

	group := nomadTaskGroup{
		Name:  "session",
		Count: 1,
		// GC is the only self-healer for session jobs: a dead
		// agent must produce a terminal alloc that STAYS terminal, so both
		// in-place restart and reschedule are disabled here.
		RestartPolicy: nomadRestartPolicy{Attempts: 0, Mode: "fail"},
		ReschedulePolicy: nomadReschedulePolicy{
			Attempts:  0,
			Unlimited: false,
		},
		// disconnect.replace=false governs only the unknown-client window;
		// after lost_after the zeroed reschedule block above is what
		// actually forbids a replacement alloc (the serial-dependency note).
		Disconnect: &nomadDisconnect{
			Replace:   boolPtr(false),
			Reconcile: "keep_original",
			LostAfter: lostAfter.Nanoseconds(),
		},
		Networks: networks,
		Tasks:    tasks,
	}
	return group
}

func sessionTask(agentImage string) nomadTask {
	// The image owns the agent toolchain. Keep the task alive with a
	// signal-aware native sleep process; GC's rendered command and runtime
	// environment still arrive later through alloc-exec/staging. This Docker
	// task is the only session shape there is.
	task := nomadTask{
		Name:   "agent",
		Driver: "docker",
		Config: map[string]any{
			// The image is resolved from dispatch metadata, not frozen here.
			//
			// Baking the literal made the parent job image-specific, so one
			// deployment served exactly one image and two lanes wanting
			// different agent toolchains could not coexist on this runtime at
			// all. The parent now carries the deployment default
			// in Meta and a dispatch may override it per session, which is
			// what lets a lane onboard by naming its runtime rather than by
			// standing up a second deployment.
			//
			// Nomad's own parameterized metadata, verified live on this
			// fleet's Nomad 2.0.5: ${NOMAD_META_*} interpolates into the
			// docker driver's config image, dispatch metadata MERGES over the
			// job's meta block rather than replacing it, and a key not
			// declared in meta_optional is refused outright. The constraint
			// stanza is the one place interpolation does NOT apply, which is
			// why the fatal placement rules stay literal above.
			"image":   "${NOMAD_META_" + agentImageMetaKey + "}",
			"command": "/bin/sleep",
			"args":    []string{"infinity"},
		},
		Resources: nomadResources{
			CPU:       sessionCPU,
			MemoryMB:  sessionMemoryMB,
			SecretsMB: sessionSecretsMB,
		},
		KillTimeout: killTimeout.Nanoseconds(),
		// No identity{env|file}, Vault, Consul, or template blocks: Nomad's
		// default workload identity is fail-closed already, and this design
		// keeps it that way as a named control — so nothing
		// is declared here.
	}
	return task
}

// --- log-shipper task ---

const (
	logShipperTaskName         = "log-shipper"
	logShipperMetricsPortLabel = "metrics"
	// These files are the allocation-local control channel between opStop
	// and the log-shipper wrapper. They live under NOMAD_ALLOC_DIR, which is
	// shared by the session task group.
	logShipperPIDFile       = ".gc-log-shipper.pid"
	logShipperFlushRequest  = ".gc-log-shipper.flush"
	logShipperFlushComplete = ".gc-log-shipper.flushed"

	// vectorVersion/vectorArchive/vectorSHA256/vectorURL pin the
	// log-shipper's own binary artifact ("vector sidecar ... pinned
	// version + sha256"). vectorSHA256 is copied verbatim from
	// vectordotdev/vector's own published
	// vector-<version>-SHA256SUMS release asset for this exact archive —
	// never computed locally — so a corrupted or substituted download
	// fails Nomad's artifact-stanza checksum verification closed rather
	// than running an unverified binary.
	vectorVersion = "0.58.0"
	vectorArchive = "vector-" + vectorVersion + "-x86_64-unknown-linux-gnu.tar.gz"
	vectorSHA256  = "a4634bea859a7ad7064ff3dd6f6ad7eb0e8dd4493cc41657d84da8dd66f09d09"
	vectorURL     = "https://github.com/vectordotdev/vector/releases/download/v" + vectorVersion + "/" + vectorArchive

	// vectorBinPath is where the artifact stanza's extraction (RelativeDest
	// "local/") lands the vector binary — the release tarball's own
	// internal layout is "vector-x86_64-unknown-linux-gnu/bin/vector",
	// unversioned, unlike the archive filename itself.
	vectorBinPath = "local/vector-x86_64-unknown-linux-gnu/bin/vector"

	// logShipperCPU/logShipperMemoryMB are vector's own resource class —
	// separate from sessionCPU/sessionMemoryMB since this task does
	// meaningfully less work than the agent it ships logs for. Sized for
	// the fake/offline lane only, same caveat as the session task's own
	// resources.
	logShipperCPU      = 100
	logShipperMemoryMB = 128

	// logShipperFlushWindow is how much longer than the agent's own
	// killTimeout the shipper gets to drain its buffer before Nomad
	// SIGKILLs it. Combined with Leader:true on the agent task
	// (sessionTaskGroup), Nomad only signals this task to stop AFTER the
	// agent task has already exited, so this window bounds just the
	// shipper's own flush, not the agent's drain too.
	logShipperFlushWindow = 15 * time.Second
	logShipperKillTimeout = killTimeout + logShipperFlushWindow
)

// logShipperWrapperScript is the log-shipper task's own command: it
// resolves the Authorization bearer token's VALUE from
// GC_LOG_SINK_TOKEN_FILE (never the job spec itself, see logShipperConfig)
// before handing off to vector, mirroring sessionTask's own /bin/sh -c
// wrapper pattern rather than invoking the fetched binary directly. It also
// publishes the vector child pid and records a successful graceful exit
// after opStop has requested the final flush.
const logShipperWrapperScript = `set -eu
pid_file="$NOMAD_ALLOC_DIR/` + logShipperPIDFile + `"
flush_request="$NOMAD_ALLOC_DIR/` + logShipperFlushRequest + `"
flush_complete="$NOMAD_ALLOC_DIR/` + logShipperFlushComplete + `"
rm -f "$pid_file" "$flush_complete"
if [ -n "${GC_LOG_SINK_TOKEN_FILE:-}" ]; then
  GC_LOG_SINK_TOKEN="$(cat "$GC_LOG_SINK_TOKEN_FILE")"
  export GC_LOG_SINK_TOKEN
fi
mkdir -p /var/lib/vector
` + vectorBinPath + ` --config local/vector.toml &
vector_pid=$!
printf '%s\n' "$vector_pid" >"$pid_file"
vector_status=0
wait "$vector_pid" || vector_status=$?
rm -f "$pid_file"
if [ -e "$flush_request" ] && [ "$vector_status" -eq 0 ]; then
  : >"$flush_complete"
fi
exit "$vector_status"`

// logShipperTask builds the log-shipper task added to the session group
// when cfg.enabled(). It shares the group's alloc dir with
// the agent task for free (every task in a Nomad group sees the same
// $NOMAD_ALLOC_DIR) — that shared dir is exactly what lets vector tail the
// agent's Nomad-captured stdout (vectorConfigTOML's session_stdout source)
// without any extra plumbing.
func logShipperTask(cfg logShipperConfig) nomadTask {
	env := map[string]string{
		"GC_LOG_SINK":   cfg.Sink,
		"GC_LOG_LABELS": cfg.Labels,
		// node.unique.name is Nomad's own job-spec-level interpolation
		// (resolved once at placement, into a literal env var) — distinct
		// from the Nomad TEMPLATE `env` function vectorConfigTOML uses to
		// read this var back out of the task's environment when it renders
		// vector.toml (vectorConfigTOML's doc comment has the full split).
		"GC_LOG_NODE_NAME": "${node.unique.name}",
	}
	if cfg.TokenFile != "" {
		env["GC_LOG_SINK_TOKEN_FILE"] = cfg.TokenFile
	}

	return nomadTask{
		Name:   logShipperTaskName,
		Driver: "exec",
		// A failed log shipper is an observability failure, not an agent
		// lifecycle failure. Keep it as a poststart sidecar and make the
		// agent the explicit group leader so Nomad does not tear the agent
		// down when this task cannot fetch or start Vector.
		Lifecycle: &nomadTaskLifecycle{Hook: "poststart", Sidecar: true},
		Config: map[string]any{
			"command": "/bin/sh",
			"args":    []string{"-c", strings.ReplaceAll(logShipperWrapperScript, "${", "$${")},
		},
		Env: env,
		Artifacts: []nomadArtifact{{
			GetterSource:  cfg.artifactSource(),
			GetterOptions: map[string]string{"checksum": "sha256:" + vectorSHA256},
			RelativeDest:  "local/",
		}},
		Templates: []nomadTemplate{{
			EmbeddedTmpl: vectorConfigTOML(cfg),
			DestPath:     "local/vector.toml",
			ChangeMode:   "noop",
		}},
		Resources: nomadResources{
			CPU:      logShipperCPU,
			MemoryMB: logShipperMemoryMB,
		},
		KillTimeout: logShipperKillTimeout.Nanoseconds(),
	}
}

func (c logShipperConfig) artifactSource() string {
	if c.Artifact != "" {
		return c.Artifact
	}
	return vectorURL
}

// vectorConfigTOML builds the log-shipper task's own vector config, rendered
// as a Nomad Template stanza (EmbeddedTmpl) BEFORE vector ever starts. Every
// value Nomad's own consul-template `env` function can see at render time —
// NOMAD_ALLOC_DIR, NOMAD_ALLOC_ID, NOMAD_META_GC_SESSION (from the dispatch
// payload's gc_session Meta key), and this task's own Env block values
// (GC_LOG_SINK, GC_LOG_LABELS, GC_LOG_NODE_NAME) — is written here as real
// Nomad template interpolation ("{{ env "VAR" }}"), the same mechanism
// proven for the prom_exporter sink's address, and extended to the rest of
// the config after a live-Nomad proof hit the sibling defect at the
// gc_log_sink URI: relying on VECTOR'S OWN "${VAR}" substitution, resolved
// from its own process environment when it starts, does not reliably work
// inside the exec-driver task's environment on real Nomad — GC_LOG_SINK
// rendered as the literal string "${GC_LOG_SINK}", which Vector rejected as
// an invalid URI, the same Exit 78 failure class already fixed for the
// port. Baking the real value into the file at Nomad render time removes
// the dependency on Vector's own env-substitution for every one of these
// fields.
//
// Two placeholders are the deliberate, narrower exception and stay on
// Vector's own "${VAR}" substitution, because neither value exists yet at
// Nomad's template-render time (which happens before the task's own command
// runs):
//   - "${GC_LOG_SINK_TOKEN}" (in the auth block) is only exported into
//     Vector's process environment by logShipperWrapperScript, immediately
//     before it execs vector — the export and the vector invocation share
//     the same shell, so the value is provably present when Vector starts.
//   - "${HOME}" is neither a Nomad-injected runtime variable nor a value
//     this task's Env block declares, so there is nothing for `env` to read
//     at render time; it relies on the exec driver populating a normal
//     POSIX process environment for Vector's own user, same as any other
//     command.
//
// Nomad's outer template pass consumes "${...}" expressions unless the
// dollar is doubled, so the returned template escapes both remaining
// placeholders for that pass; Vector receives the single-dollar form and
// resolves it from its own runtime environment. The "{{ env ... }}" calls
// above use Nomad's own delimiter rather than a dollar sign, so the escaping
// ReplaceAll below leaves them untouched (Nomad's template pass is meant to
// consume them).
//
// The session_stdout source declares two patterns rather than one, because
// two different names can appear under {{ env "NOMAD_ALLOC_DIR" }}/logs and
// only the second was ever listed:
//   - "logs/agent.stdout" — the file the deployment bootstrap actually writes
//     (ops/nomad/agent-launch.sh taps the pane with `tmux pipe-pane -o
//     "cat >> '$LOGS/agent.stdout'"`, unsuffixed) and the exact name the
//     stop-path egress copies out (runtime/ops.go's egressFiles). Vector's
//     glob treats the "." in "agent.stdout.*" as a literal, so the old single
//     pattern demanded a suffix and never matched the shipped file: the pane
//     capture was written, egressed, and silently never shipped.
//   - "logs/agent.stdout.*" — Nomad's rotation of the "agent" task's own
//     stdout file (agent.stdout.1, .2, ...). Kept: same stream, and a file can
//     match at most one of the two patterns, so declaring both is free.
//
// The sibling capture channels get their own sources rather than extra
// patterns bolted onto session_stdout:
//   - session_stderr — logs/agent.stderr, the launcher's `"$AGENT_CMD"
//     2>>"$LOGS/agent.stderr"`, plus the same .N rotation pattern the stdout
//     source uses for the task's own stream.
//   - agent_exit — logs/agent.exit, the "exit= started= ended= ..." record.
//     ONE exact name, no glob: the launcher writes it as agent.exit.$$ and
//     renames it into place, so a wildcard would only ever widen the match
//     onto the transient file. Every shipper path is a named file inside the
//     allocation's logs dir — the same credential boundary runtime/ops.go's
//     egressFiles is (a recursive sweep there would reach the staged secrets),
//     and no source here uses "**".
//
// All three feed the one label transform and the one gc_log_sink, so the
// stderr and exit channels carry the same session_name/alloc_id/node labels
// as the stdout they were captured beside.
//
// The auth block is only emitted when cfg.TokenFile is set — vector has no
// bearer-token-file primitive of its own, so an unset token file means an
// unauthenticated sink rather than a literal empty bearer token.
func vectorConfigTOML(cfg logShipperConfig) string {
	var authBlock string
	if cfg.TokenFile != "" {
		authBlock = "\n\n[sinks.gc_log_sink.auth]\nstrategy = \"bearer\"\ntoken = \"${GC_LOG_SINK_TOKEN}\"\n"
	}
	config := `[sources.session_jsonl]
type = "file"
include = ["{{ env "NOMAD_ALLOC_DIR" }}/data/*.jsonl", "${HOME}/.claude/projects/**/*.jsonl"]
read_from = "beginning"
ignore_checkpoints = true

[sources.session_stdout]
type = "file"
include = ["{{ env "NOMAD_ALLOC_DIR" }}/logs/agent.stdout", "{{ env "NOMAD_ALLOC_DIR" }}/logs/agent.stdout.*"]
read_from = "beginning"
ignore_checkpoints = true

[sources.session_stderr]
type = "file"
include = ["{{ env "NOMAD_ALLOC_DIR" }}/logs/agent.stderr", "{{ env "NOMAD_ALLOC_DIR" }}/logs/agent.stderr.*"]
read_from = "beginning"
ignore_checkpoints = true

[sources.agent_exit]
type = "file"
include = ["{{ env "NOMAD_ALLOC_DIR" }}/logs/agent.exit"]
read_from = "beginning"
ignore_checkpoints = true

[sources.internal_metrics]
type = "internal_metrics"

[transforms.label]
type = "remap"
inputs = ["session_jsonl", "session_stdout", "session_stderr", "agent_exit"]
source = '''
.session_name = "{{ env "NOMAD_META_GC_SESSION" }}"
.alloc_id = "{{ env "NOMAD_ALLOC_ID" }}"
.node = "{{ env "GC_LOG_NODE_NAME" }}"
.runtime = "nomad"
extra, err = parse_key_value("{{ env "GC_LOG_LABELS" }}", key_value_delimiter: "=", field_delimiter: ",")
if err == null {
  . = merge!(., extra)
}
'''

[sinks.gc_log_sink]
type = "http"
inputs = ["label"]
uri = "{{ env "GC_LOG_SINK" }}"
encoding.codec = "json"
framing.method = "newline_delimited"

[sinks.prom_exporter]
type = "prometheus_exporter"
inputs = ["internal_metrics"]
address = "0.0.0.0:{{ env "NOMAD_PORT_metrics" }}"
`
	return strings.ReplaceAll(config+authBlock+"\n", "${", "$${")
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
	// Meta carries jobspecHashMetaKey — the parent job's own
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
	// v2.0.5; fakenomad never validated this, so the drift
	// was invisible to the offline L1 suite until run against a real
	// cluster).
	LostAfter int64 `json:"LostAfter,omitempty"`
}

type nomadNetwork struct {
	Mode string `json:"Mode"`
	// DNS is the group's network.DNS stanza: the resolver addresses Nomad
	// writes into the alloc's /etc/resolv.conf. nil declares nothing, which
	// leaves a bridge alloc inheriting the CLIENT HOST's resolv.conf — on a
	// systemd-resolved host that is the 127.0.0.53 stub, unreachable from the
	// alloc netns, so every name lookup in the box dies in ~1ms.
	DNS *nomadDNS `json:"DNS,omitempty"`
	// DynamicPorts declares group-local ports every task in the group sees
	// as $NOMAD_PORT_<Label> — only populated when the log-shipper task
	// exists to bind one (the shipper's prometheus_exporter port).
	DynamicPorts []nomadPort `json:"DynamicPorts,omitempty"`
}

type nomadPort struct {
	Label string `json:"Label"`
}

// nomadDNS is the network.DNS subset this pack renders. Only Servers is used;
// Searches/Options stay undeclared so the stanza stays exactly as wide as the
// knob that sets it.
type nomadDNS struct {
	Servers []string `json:"Servers,omitempty"`
}

type nomadTask struct {
	Name   string `json:"Name"`
	Driver string `json:"Driver"`
	// Leader, when true, makes Nomad stop every OTHER task in the group
	// only after this task exits (the "kill_timeout ordering" requirement —
	// see sessionTaskGroup). Only ever set on the agent task, and only
	// when a log-shipper task exists to order against.
	Leader      bool                `json:"Leader,omitempty"`
	Lifecycle   *nomadTaskLifecycle `json:"Lifecycle,omitempty"`
	Config      map[string]any      `json:"Config,omitempty"`
	Env         map[string]string   `json:"Env,omitempty"`
	Artifacts   []nomadArtifact     `json:"Artifacts,omitempty"`
	Templates   []nomadTemplate     `json:"Templates,omitempty"`
	Resources   nomadResources      `json:"Resources"`
	KillTimeout int64               `json:"KillTimeout,omitempty"`
}

// nomadTaskLifecycle is the task lifecycle subset needed to keep the
// log-shipper failure domain separate from the agent task. Nomad's
// poststart+sidecar form starts the shipper after the group leader and does
// not make the sidecar's own failure a reason to stop the leader.
type nomadTaskLifecycle struct {
	Hook    string `json:"Hook,omitempty"`
	Sidecar bool   `json:"Sidecar,omitempty"`
}

type nomadResources struct {
	CPU       int `json:"CPU"`
	MemoryMB  int `json:"MemoryMB"`
	SecretsMB int `json:"SecretsMB,omitempty"`
}

// nomadArtifact mirrors Nomad's artifact stanza (go-getter): GetterOptions
// carries the "checksum" key logShipperTask uses to pin vector's binary
// download to its published sha256 ("pinned version + sha256").
type nomadArtifact struct {
	GetterSource  string            `json:"GetterSource"`
	GetterOptions map[string]string `json:"GetterOptions,omitempty"`
	RelativeDest  string            `json:"RelativeDest,omitempty"`
}

// nomadTemplate mirrors Nomad's template stanza. logShipperTask uses only
// the EmbeddedTmpl+DestPath+ChangeMode subset — see vectorConfigTOML's doc
// comment for why EmbeddedTmpl is used as a static file-delivery mechanism
// here rather than for its consul-template interpolation power.
type nomadTemplate struct {
	EmbeddedTmpl string `json:"EmbeddedTmpl"`
	DestPath     string `json:"DestPath"`
	ChangeMode   string `json:"ChangeMode,omitempty"`
}
