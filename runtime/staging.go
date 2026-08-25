package main

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
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
