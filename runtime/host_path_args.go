package main

import (
	"sort"
	"strings"
)

// hostPathReportName is the file the box writes the answer to, beside the exit
// record in the writable capture dir.
const hostPathReportName = "agent.paths"

// absolutePathArgs returns the absolute filesystem paths appearing in a
// rendered agent command's arguments, deduplicated and ordered.
//
// It exists because image_env.go's rule — an orchestrator path is not data in
// the box, it is a breakage — was enforced only for the ENVIRONMENT. A host
// path riding inside an argument (`-c key=/abs/path`, `--config /abs/path`, a
// bare positional) crossed untouched, and the agent died with an error naming
// neither the path nor the argument it came from.
//
// Deliberately conservative, because a false report is worse than none: it
// trains the reader to skip the file. Only tokens that begin with "/" (a
// relative path is resolved against the cwd the pack sets inside the box, so
// it is not an orchestrator leak), only the value half of a key=value token,
// and nothing containing a shell metacharacter — a token the shell would have
// rewritten before the agent saw it is not a path this can reason about.
func absolutePathArgs(command string) []string {
	seen := map[string]bool{}
	for _, field := range strings.Fields(command) {
		token := field
		// key=value: the path is the value. Split on the FIRST "=" so a value
		// that itself contains "=" survives.
		if i := strings.Index(token, "="); i >= 0 {
			token = token[i+1:]
		}
		if !strings.HasPrefix(token, "/") || token == "/" {
			continue
		}
		if strings.ContainsAny(token, `'"$*?[]{}()<>|&;`+"`") {
			continue
		}
		seen[token] = true
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// hostPathProbe renders the shell that asks the BOX whether each path exists,
// recording only the MISSING ones. The box is the only place that can answer
// the question, and a line in the report therefore always means something is
// wrong.
//
// A filesystem path is not a credential, which is what makes this reportable
// at all: the exit record carries argv0 and never argument VALUES, and this
// stays inside that rule by reporting paths only, and only absent ones.
func hostPathProbe(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(`for __gc_p in `)
	for i, p := range paths {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(shellQuote(p))
	}
	b.WriteString(`; do [ -e "$__gc_p" ] || printf 'missing-path: %s\n' "$__gc_p" >> "$__gc_cap/` + hostPathReportName + `"; done 2>/dev/null; `)
	return b.String()
}
