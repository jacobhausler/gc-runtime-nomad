package main

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// stageFile is one workspace file or secret to place inside a session's
// alloc (04 §5 data contract: "CopyFiles/overlays ride the same channel" as
// workspace tar staging). Path is always relative to the extraction root
// the caller picks (WorkDir for workspace files, NOMAD_SECRETS_DIR for
// secrets) — never absolute, never containing "..".
type stageFile struct {
	Path    string `json:"path"`
	Content []byte `json:"content"` // json.Marshal/Unmarshal base64-encode []byte automatically
	Mode    int64  `json:"mode,omitempty"`
}

// stageConfig is the wire "start config" this pack reads from stdin for
// `start`/`provision` (04 §5: "the wire start config is a SUBSET of
// runtime.Config"). The exact field names are this pack's own decision, not
// a transcription of gc's internal wire schema (RUNTIME-RPP-002 pins that
// schema at internal/runtime/exec/json.go, which lives in the gascity repo
// this zero-dependency pack cannot import) — it carries exactly the subset
// 04 §5 describes as in-scope for staging: WorkDir + Env + Files (the
// CopyFiles/overlay analog; PackOverlayDirs/OverlayDir merging is out of
// scope for this bead). An empty/absent stdin body decodes to the zero
// value, which stage treats as a no-op — start/provision keep working
// exactly as before staging landed when a caller sends no config.
type stageConfig struct {
	WorkDir string            `json:"work_dir,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Files   []stageFile       `json:"files,omitempty"`

	// The launch/priming half of the same `start` payload (cr-gdeav.6.10).
	// The fields above are the STAGING subset this pack originally read;
	// these are the ones gc's marshalStartConfig
	// (internal/runtime/exec/json.go:26-67) has been sending all along and
	// this pack dropped on the floor — which is why an ordinary
	// nomad-canary launch produced a live agent that nothing ever primed
	// (cr-gdeav.6.9 receipt §1.4: "command, nudge, process_names, ready_*
	// are silently discarded"). Field names are gc's, verbatim, and
	// startup_test.go pins them against gc's own struct, so a rename
	// upstream is a test failure here rather than another silent drop.
	Command           string   `json:"command,omitempty"`
	Nudge             string   `json:"nudge,omitempty"`
	ProcessNames      []string `json:"process_names,omitempty"`
	ReadyPromptPrefix string   `json:"ready_prompt_prefix,omitempty"`
	ReadyDelayMs      int      `json:"ready_delay_ms,omitempty"`

	// PromptSuffix / PromptFlag are named for payload gc does NOT serialize
	// today: both exist on runtime.Config
	// (internal/runtime/runtime.go:660-671) and the native tmux provider
	// consumes them in-process (internal/runtime/tmux/adapter.go:1462-1483),
	// but marshalStartConfig has no such key — so under the default
	// `prompt_mode=arg` an RPP lane CANNOT receive the startup prompt on
	// argv at all. That is the gc-side gap the EDGE twin carries
	// (cr-gdeav.6.9 receipt §4). Decoding them costs nothing and lets the
	// pack pick the prompt up with no further change if upstream fix (a) —
	// put them on the wire — lands; until then the provider must set
	// `prompt_mode = "none"` so the prompt arrives in Nudge.
	PromptSuffix string `json:"prompt_suffix,omitempty"`
	PromptFlag   string `json:"prompt_flag,omitempty"`
}

// readStageConfig decodes a stageConfig from stdin. Empty/whitespace-only
// input is the zero value (no error) — the pre-staging calling convention
// for start/provision (no stdin body at all).
func readStageConfig(stdin io.Reader) (stageConfig, error) {
	data, err := io.ReadAll(stdin)
	if err != nil {
		return stageConfig{}, fmt.Errorf("reading start config: %w", err)
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return stageConfig{}, nil
	}
	var cfg stageConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return stageConfig{}, fmt.Errorf("decoding start config: %w", err)
	}
	return cfg, nil
}

// startupPrompt returns the text this pack owes the launched pane as its
// first turn, and which wire field it came from. Empty text means nothing is
// owed — there was no startup prompt, or gc routed it to argv (the default
// `prompt_mode=arg`), which it cannot deliver to an RPP lane.
//
// Nudge wins: it is the field gc actually sends, and under
// `prompt_mode="none"` the startup prompt is already prepended to the
// agent's configured nudge (cmd/gc/prompt_delivery.go:43-45). PromptSuffix is
// consulted only as the forward-compat branch documented on the field above.
func (c stageConfig) startupPrompt() (text, source string) {
	if strings.TrimSpace(c.Nudge) != "" {
		return c.Nudge, "nudge"
	}
	if strings.TrimSpace(c.PromptSuffix) != "" {
		return unshellQuote(c.PromptSuffix), "prompt_suffix"
	}
	return "", ""
}

// promptSubset returns only the startup-prompt/priming fields of cfg — never
// WorkDir, Env, or Files — for persisting onto a session's binding
// (cr-gdeav.6.12). It exists because gc's proc.provision path carries these
// fields on opProvisionWithConfig's stageConfig, but opExec's transport-launch
// path (markTransportLaunched, ops.go) runs with no stageConfig of its own —
// gc's canonical tmux launch argv carries only the transport handshake — so
// the prompt fields have to survive from stage time to whichever launch path
// actually runs. Excluding Env/Files keeps that persisted copy from ever
// re-carrying a secret the staging call routed past the sidecar entirely.
func (c stageConfig) promptSubset() stageConfig {
	return stageConfig{
		Command:           c.Command,
		Nudge:             c.Nudge,
		ProcessNames:      c.ProcessNames,
		ReadyPromptPrefix: c.ReadyPromptPrefix,
		ReadyDelayMs:      c.ReadyDelayMs,
		PromptSuffix:      c.PromptSuffix,
		PromptFlag:        c.PromptFlag,
	}
}

// unshellQuote decodes one single-quoted shell word back to its value — the
// inverse of this file's shellQuote, and the same quote-escaping grammar
// parseExecWireArgv already decodes on the exec wire. gc builds PromptSuffix
// with shellquote.Quote, so an undecoded suffix would type literal quote
// characters into the agent. Anything outside that grammar is returned
// verbatim rather than guessed at.
func unshellQuote(word string) string {
	if len(word) < 2 || word[0] != '\'' {
		return word
	}
	rest := word[1:]
	var b strings.Builder
	for rest != "" {
		end := strings.IndexByte(rest, '\'')
		if end < 0 {
			return word
		}
		b.WriteString(rest[:end])
		rest = rest[end+1:]
		if rest == "" {
			return b.String()
		}
		if !strings.HasPrefix(rest, `\'`) {
			return word
		}
		b.WriteByte('\'')
		rest = rest[2:]
		if rest == "" || rest[0] != '\'' {
			return word
		}
		rest = rest[1:]
	}
	return word
}

// envArgvSafeNames is this pack's reimplementation of gc's envArgvSafe
// allow-list (E1a §4.5: "an ALLOW list of names whose values may ride argv
// — locale/terminal, city/rig/agent identity, session identity+epochs,
// derived paths; anything unlisted is presumed credential"). The pack
// cannot import gc's envsecret.go (zero-gascity-dependency contract), so
// this is a faithful-but-independent classification pinned to those same
// categories, not a byte-identical copy. GC_INSTANCE_TOKEN is deliberately
// NOT here even though it is identity-adjacent: E1a §4.5 calls it out as a
// CAPABILITY, not an identifier, and capabilities are exactly what argv
// (world-readable via /proc/<pid>/cmdline) must never carry.
var envArgvSafeNames = map[string]bool{
	"LANG":     true,
	"LC_ALL":   true,
	"LC_CTYPE": true,
	"TERM":     true,
	"TZ":       true,
	"PATH":     true,

	"GC_CITY":          true,
	"GC_RIG":           true,
	"GC_AGENT":         true,
	"GC_SESSION":       true,
	"GC_SESSION_EPOCH": true,
	"GC_WORKDIR":       true,
	"GC_HOME_DIR":      true,
}

// envArgvSafe reports whether key's value is safe to carry on argv (a tmux
// `new-session -e KEY=VALUE`) rather than requiring file-based secrets-dir
// delivery. See envArgvSafeNames.
func envArgvSafe(key string) bool {
	return envArgvSafeNames[key]
}

// mergeExtraSecretEnv builds the staging-only env stage() uses to
// materialize secret files: base (cfg.Env) plus, for each name in keys not
// already present in base, that name's value read from this process's own
// environment — the supervisor's process env, set via ~/.gc/secrets.env
// passthrough, bridging a value gc's own city.toml provider env map has no
// way to interpolate (GC_NOMAD_EXTRA_SECRET_KEYS, cr-u4plc.2).
// Caller-supplied base always wins on collision. The returned set marks
// which keys came from this merge so stage() can always route them to the
// secrets dir even if their name coincidentally matches envArgvSafeNames —
// an extra secret key is always secret by declaration, never by lucky
// allow-listing. This merge is local to stage() and never mutates base
// itself, so an extra-sourced value can never reach buildLaunchCommand's
// argv (buildLaunchCommand is only ever called with cfg.Env, untouched by
// this function).
func mergeExtraSecretEnv(base map[string]string, keys []string) (map[string]string, map[string]bool) {
	merged := make(map[string]string, len(base)+len(keys))
	for k, v := range base {
		merged[k] = v
	}
	extra := make(map[string]bool, len(keys))
	for _, k := range keys {
		if _, ok := merged[k]; ok {
			continue
		}
		v, ok := os.LookupEnv(k)
		if !ok {
			continue
		}
		merged[k] = v
		extra[k] = true
	}
	return merged, extra
}

// stripLocalCityStoreEnv removes the local target/store family when a
// session is explicitly staged for a remote city. gc's remote selector is
// intentionally fail-closed when these families coexist: preserving local
// identity in a remote allocation would make every nested gc invocation
// reject the otherwise valid remote target. Keep the remote city name, actor,
// and session identity keys; they are not local target selectors.
func stripLocalCityStoreEnv(env map[string]string) map[string]string {
	if strings.TrimSpace(env["GC_CITY_URL"]) == "" && strings.TrimSpace(env["GC_CITY_CONTEXT"]) == "" {
		return env
	}
	filtered := make(map[string]string, len(env))
	for key, value := range env {
		switch {
		case key == "GC_CITY", key == "GC_CITY_PATH", key == "GC_CITY_ROOT", key == "GC_CITY_RUNTIME_DIR":
			continue
		case strings.HasPrefix(key, "GC_BEADS"):
			continue
		default:
			filtered[key] = value
		}
	}
	return filtered
}

const stagedEnvScriptName = "env.sh"

// validShellEnvName accepts the portable shell identifier form used by
// `export NAME=value`. Invalid names remain available through their existing
// individual secret file, but are not emitted into env.sh where they could
// turn a value into shell syntax.
func validShellEnvName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c == '_' {
			continue
		}
		if i == 0 || c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// shellQuote returns a single-quoted shell word. The generated env.sh is
// sourced inside an allocation, so values must remain data even when they
// contain spaces, quotes, substitutions, or command separators.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

// pemPointerSuffix names the mTLS identity bridge convention (cr-gdeav.6.15):
// an extra secret key GC_NOMAD_EXTRA_SECRET_KEYS bridges in named FOO_PEM is
// materialized as a file exactly like any other staged secret (stage()'s
// per-key stageFile loop, unchanged), but its BYTES never ride env.sh — only
// a POINTER to where the file landed does, exported as FOO. This is what lets
// a consumer that wants a file path — gc's remote client reads
// GC_CITY_CA/GC_CITY_CLIENT_CERT/GC_CITY_CLIENT_KEY as file paths
// (cmd/gc/remote_client.go:92-93) — get exactly that, with nothing beyond the
// Nomad-provided $NOMAD_SECRETS_DIR path (not a secret, paneStagedEnvPrelude's
// own reasoning) ever appearing in env.sh for these keys.
const pemPointerSuffix = "_PEM"

// pemPointerName returns the env var buildEnvScript should point at key's
// staged file, and whether key follows the *_PEM convention at all.
func pemPointerName(key string) (string, bool) {
	base := strings.TrimSuffix(key, pemPointerSuffix)
	if base == key || base == "" || !validShellEnvName(base) {
		return "", false
	}
	return base, true
}

// buildEnvScript creates the sourceable environment contract staged into
// NOMAD_SECRETS_DIR. Keys are sorted for deterministic receipts and only
// valid shell identifiers are exported. A *_PEM key's own value is withheld
// (pemPointerName above) — env.sh points AT its staged file instead of ever
// repeating the PEM bytes as a second copy.
func buildEnvScript(env map[string]string) []byte {
	keys := make([]string, 0, len(env))
	for key := range env {
		if validShellEnvName(key) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, key := range keys {
		if _, isPointer := pemPointerName(key); isPointer {
			continue
		}
		b.WriteString("export ")
		b.WriteString(key)
		b.WriteByte('=')
		b.WriteString(shellQuote(env[key]))
		b.WriteByte('\n')
	}
	for _, key := range keys {
		pointer, ok := pemPointerName(key)
		if !ok {
			continue
		}
		// Double-quoted (not shellQuote's single quotes) so $NOMAD_SECRETS_DIR
		// expands at source time to the real staged path every time env.sh is
		// sourced - never a path this pack hardcodes or resolves itself.
		b.WriteString("export ")
		b.WriteString(pointer)
		b.WriteString(`="$NOMAD_SECRETS_DIR/`)
		b.WriteString(key)
		b.WriteString("\"\n")
	}
	return []byte(b.String())
}

// Remote-context staging (cr-gdeav.6.15): a Nomad-launched Codex box can
// reach the city over its own native TLS/context mechanism — no flags, no
// bespoke wiring — if this pack stages a GC_HOME with a contexts.toml gc's
// own resolver already knows how to read (cmd/gc/remote_target.go:104-106,
// $GC_HOME/contexts.toml; schema at internal/clientcontext/clientcontext.go).
// This pack cannot import that package (zero-gascity-dependency contract), so
// the TOML is hand-written here, pinned to the exact field names
// (name/url/ca_file/client_cert_file/client_key_file) rather than guessed at.
const (
	// remoteContextRelPath is where the staged GC_HOME lands under
	// NOMAD_SECRETS_DIR - a subdirectory, not the secrets root itself, so gc's
	// OWN contexts.toml never collides with the per-key secret files
	// staged beside it.
	remoteContextDir     = "gc-home"
	remoteContextRelPath = remoteContextDir + "/contexts.toml"

	// nomadSecretsDirPath is Nomad's own fixed exec-task secrets mount point
	// (verified live: `nomad alloc exec ... env` reports
	// NOMAD_SECRETS_DIR=/secrets on every task) - a Nomad platform constant,
	// not a value this pack invents or reads from the wire. contexts.toml is
	// a literal TOML file with no shell variable expansion, unlike env.sh, so
	// its ca_file/client_cert_file/client_key_file fields need the resolved
	// path written out; env.sh's own pointer exports (buildEnvScript above)
	// stay dynamic ($NOMAD_SECRETS_DIR) precisely because a shell CAN expand
	// that at source time and TOML cannot.
	nomadSecretsDirPath = "/secrets"
)

// remoteContextPEMKeys names the three identity roles this bridge wires into
// a context, and the *_PEM secret key each expects (cr-gdeav.6.15, matching
// what the executive stages via GC_NOMAD_EXTRA_SECRET_KEYS). Order is the
// order their fields appear in the rendered TOML.
var remoteContextPEMKeys = [3]struct{ field, key string }{
	{"ca_file", "GC_CITY_CA_PEM"},
	{"client_cert_file", "GC_CITY_CLIENT_CERT_PEM"},
	{"client_key_file", "GC_CITY_CLIENT_KEY_PEM"},
}

// buildRemoteContextFile returns the gc-home/contexts.toml stageFile for
// secretEnv, or nil when secretEnv carries no remote target to stage
// (missing GC_CITY_NAME or GC_CITY_URL) — a no-op for every deployment that
// does not opt into this bridge, same invariant every other staging field on
// this pack already holds.
func buildRemoteContextFile(secretEnv map[string]string) *stageFile {
	name := strings.TrimSpace(secretEnv["GC_CITY_NAME"])
	url := strings.TrimSpace(secretEnv["GC_CITY_URL"])
	if name == "" || url == "" {
		return nil
	}
	var b strings.Builder
	b.WriteString("[[context]]\n")
	fmt.Fprintf(&b, "name = %s\n", tomlQuote(name))
	fmt.Fprintf(&b, "url = %s\n", tomlQuote(url))
	for _, role := range remoteContextPEMKeys {
		if _, present := secretEnv[role.key]; !present {
			continue
		}
		fmt.Fprintf(&b, "%s = %s\n", role.field, tomlQuote(nomadSecretsDirPath+"/"+role.key))
	}
	return &stageFile{Path: remoteContextRelPath, Content: []byte(b.String()), Mode: 0o600}
}

// buildRemoteContextEnvLines returns the extra env.sh lines that point gc's
// own remote-context resolution at the GC_HOME this pack stages
// (buildRemoteContextFile) - nil when no context was staged (the same
// missing-name-or-url no-op), so an ordinary deployment's env.sh is
// unaffected. GC_HOME is exported dynamically ($NOMAD_SECRETS_DIR/gc-home) —
// same reasoning as the *_PEM pointers above, since a literal path this pack
// resolved itself would assume Nomad's mount point rather than reading it;
// GC_CITY_CONTEXT is a context NAME, not a credential, so it still goes
// through shellQuote like any ordinary exported value.
func buildRemoteContextEnvLines(secretEnv map[string]string) []byte {
	name := strings.TrimSpace(secretEnv["GC_CITY_NAME"])
	url := strings.TrimSpace(secretEnv["GC_CITY_URL"])
	if name == "" || url == "" {
		return nil
	}
	var b strings.Builder
	b.WriteString(`export GC_HOME="$NOMAD_SECRETS_DIR/` + remoteContextDir + "\"\n")
	b.WriteString("export GC_CITY_CONTEXT=" + shellQuote(name) + "\n")
	return []byte(b.String())
}

// remoteContextEnvScriptInput removes the provider URL from env.sh once the
// staged gc context owns it. Keeping both GC_CITY_URL and GC_CITY_CONTEXT
// would select two remote targets and make gc fail closed; the URL remains in
// contexts.toml, where the context resolver reads it together with the mTLS
// file paths. Deployments without a complete staged context keep the original
// environment unchanged.
func remoteContextEnvScriptInput(secretEnv map[string]string) map[string]string {
	if strings.TrimSpace(secretEnv["GC_CITY_NAME"]) == "" || strings.TrimSpace(secretEnv["GC_CITY_URL"]) == "" {
		return secretEnv
	}
	filtered := make(map[string]string, len(secretEnv))
	for key, value := range secretEnv {
		if key != "GC_CITY_URL" {
			filtered[key] = value
		}
	}
	return filtered
}

// tomlQuote renders s as a TOML basic string. contexts.toml carries only a
// city name, a URL, and this pack's own fixed secrets-dir file paths - none
// expected to need escaping - but this is written defensively rather than
// assuming that stays true.
func tomlQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// errStagePathInvalid marks a stageFile whose Path is absolute or escapes
// its destination directory via ".." — buildTar refuses to archive it
// rather than producing a tar an extraction could use to write outside the
// intended workdir/secrets-dir root.
var errStagePathInvalid = errors.New("staged file path is invalid")

func validateStagePath(path string) error {
	if path == "" {
		return fmt.Errorf("%w: empty path", errStagePathInvalid)
	}
	if filepath.IsAbs(path) {
		return fmt.Errorf("%w: %q is absolute", errStagePathInvalid, path)
	}
	clean := filepath.ToSlash(filepath.Clean(path))
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("%w: %q escapes its destination directory", errStagePathInvalid, path)
	}
	return nil
}

// buildTar archives files into a tar byte stream suitable for
// `tar -x -f - -C <dir>` over exec-stdin (04 §5: "staging = tar stream over
// exec stdin from the controller"). Every path is validated first so a
// caller-supplied Path can never place a file outside the extraction root.
func buildTar(files []stageFile) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range files {
		if err := validateStagePath(f.Path); err != nil {
			return nil, err
		}
		mode := f.Mode
		if mode == 0 {
			mode = 0o644
		}
		hdr := &tar.Header{
			Name:     f.Path,
			Mode:     mode,
			Size:     int64(len(f.Content)),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, fmt.Errorf("tar header for %q: %w", f.Path, err)
		}
		if _, err := tw.Write(f.Content); err != nil {
			return nil, fmt.Errorf("tar content for %q: %w", f.Path, err)
		}
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("closing tar: %w", err)
	}
	return buf.Bytes(), nil
}
