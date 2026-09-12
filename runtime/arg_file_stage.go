package main

import (
	"os"
	"path/filepath"
	"strings"
)

// argFileStageMaxBytes bounds what an argument can drag into a box. A model
// catalog or a config file is kilobytes; anything approaching this is not
// agent configuration and does not belong in a start path that gc times.
const argFileStageMaxBytes = 1 << 20

// argFileStageDenied are the path prefixes an argument can never pull from,
// relative to the user's home. A credential reaches a box through the secrets
// channel, which has its own 0600 discipline and its own audit; it must not
// also arrive because it happened to appear in argv.
var argFileStageDenied = []string{
	".gc/secrets.env",
	".gc/run",
	".ssh",
	".aws",
	".config/gh",
}

// argFileStageAllowed reports whether a path named by an agent argument may be
// copied into the box. Deny by prefix on the resolved path, so a symlink or a
// "..' cannot walk into a denied tree.
func argFileStageAllowed(path string) bool {
	resolved := path
	if r, err := filepath.EvalSymlinks(path); err == nil {
		resolved = r
	}
	resolved = filepath.Clean(resolved)
	home, err := os.UserHomeDir()
	if err != nil {
		// No home to compare against: refuse rather than guess. A start that
		// misses a config file is diagnosable (the box reports the missing
		// path); a credential copied into a box is not recoverable.
		return false
	}
	for _, deny := range argFileStageDenied {
		full := filepath.Clean(filepath.Join(home, deny))
		if resolved == full || strings.HasPrefix(resolved, full+string(os.PathSeparator)) {
			return false
		}
	}
	return true
}

// argFilesToStage returns the files named by absolute paths in a rendered
// agent command that this orchestrator can read and should place inside the
// box at the same path.
//
// Everything it skips, it skips silently: a missing, unreadable, oversized or
// denied path is reported by the BOX instead (host_path_args.go writes
// agent.paths and prime.go surfaces it), and failing the launch here would
// turn a diagnosable condition into a session that never starts.
func argFilesToStage(command string) []stageFile {
	var out []stageFile
	for _, path := range absolutePathArgs(command) {
		if !argFileStageAllowed(path) {
			continue
		}
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() > argFileStageMaxBytes {
			continue
		}
		content, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		out = append(out, stageFile{Path: path, Content: content, Mode: 0o600})
	}
	return out
}

// argFileStageCommand is the in-box shell that writes ONE argument-named file
// at its absolute path, reading the bytes from stdin.
//
// Its own exec, because neither existing tar can place an absolute path: the
// workspace tar extracts at the work dir and the secrets tar at the secrets
// dir. The path is passed as an ARGUMENT and never interpolated into the
// program text — it is operator-supplied data, and a path carrying a shell
// metacharacter would otherwise be code.
func argFileStageCommand(path string) []string {
	return []string{
		"/bin/sh", "-c",
		`mkdir -p "$(dirname "$1")" && cat > "$1" && chmod 600 "$1"`,
		"stage-arg-file", path,
	}
}
