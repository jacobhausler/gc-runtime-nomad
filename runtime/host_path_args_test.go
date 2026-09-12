package main

import (
	"strings"
	"testing"
)

// THE DEFECT CLASS THIS NAMES, measured 2026-09-12. The lane's agent exited 1
// within the same second, every time, for days. The exit record finally said
// argv0=codex, and reproducing codex in the box gave:
//
//	$ codex exec -c model_catalog_json=/orchestrator-home/.agent/model-catalogs/catalog.json "say hi"
//	Error: No such file or directory (os error 2)
//
// The provider's args carried an ORCHESTRATOR path. image_env.go already owns
// this exact class for the ENVIRONMENT — PATH, HOME, TMPDIR, XDG_*, GC_HOME
// are stripped because "a path that exists only on the orchestrator is not
// data there, it is a breakage" — but a host path inside an argument
// (-c key=/abs/path) crosses untouched, and the agent dies with an error that
// names neither the path nor the argument.
//
// The box is the only place that can answer "does this path exist here", so
// the box answers it. A filesystem path is not a credential, which is what
// makes this reportable at all: the exit record deliberately carries argv0 and
// never argument VALUES, and this stays within that rule by reporting only
// paths, and only the ones that are MISSING.

func TestPaneReportsArgumentPathsMissingInTheBox(t *testing.T) {
	cmd := buildLaunchCommandForPane(map[string]string{}, "",
		`exec codex exec -c model_catalog_json=/orchestrator-home/.agent/cat.json -c model=x /opt/present`)
	body := launchPaneBody(t, cmd)
	if !strings.Contains(body, "/orchestrator-home/.agent/cat.json") {
		t.Fatalf("the absolute path in an argument is never checked in the box, so a path that exists only on the orchestrator kills the agent with an error naming neither:\n%s", body)
	}
	if !strings.Contains(body, "/opt/present") {
		t.Errorf("bare absolute-path arguments must be checked too, not only key=/path ones:\n%s", body)
	}
	// Non-paths must not be probed: a model name or a flag is not a file, and
	// reporting it missing would be noise that trains the reader to ignore the
	// report.
	//
	// Asserted against the PROBE, not the whole body. Checking the body for
	// "model=x" can never pass — the body contains the agent's full command,
	// so the token is always present. An assertion that cannot pass is not a
	// guard.
	probe := body[strings.Index(body, "for __gc_p in "):]
	probe = probe[:strings.Index(probe, "; done")]
	if strings.Contains(probe, "model=x") || strings.Contains(probe, "rtxpro") {
		t.Errorf("a non-path argument value is being probed as a file: %q", probe)
	}
	// The report goes beside the exit record, in the writable capture dir.
	if !strings.Contains(body, "agent.paths") {
		t.Errorf("there is nowhere for the box's answer to land:\n%s", body)
	}
}

// It must never report a path that DOES exist: the whole value is that a line
// in this file means something is wrong.
func TestOnlyMissingPathsAreReported(t *testing.T) {
	snippet := hostPathProbe([]string{"/a/missing", "/b/also"})
	if !strings.Contains(snippet, "-e") {
		t.Errorf("the probe does not test existence, so it would report every path: %q", snippet)
	}
	if !strings.Contains(snippet, "||") {
		t.Errorf("the probe must report only on the failing branch: %q", snippet)
	}
}

func TestAbsolutePathArgsExtraction(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{`codex exec -c model_catalog_json=/orchestrator-home/.agent/cat.json`, []string{"/orchestrator-home/.agent/cat.json"}},
		{`codex -c model=rtxpro6000-gateway/qwen38-flashnext-nvfp4`, nil},
		{`agent --config /etc/agent.toml /srv/work`, []string{"/etc/agent.toml", "/srv/work"}},
		{`agent --flag=value`, nil},
		{`agent 'quoted arg'`, nil},
		// A relative path is resolved against the pane's cwd, which the pack
		// sets inside the box, so it is not an orchestrator leak.
		{`agent ./local/thing`, nil},
	} {
		got := absolutePathArgs(tc.in)
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("absolutePathArgs(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// The report is worthless if the pack does not read it: an operator reads the
// pack's error, not the allocation's filesystem.
func TestPaneGoneDiagnosisNamesMissingArgumentPaths(t *testing.T) {
	l := &lifecycle{}
	got := l.hostPathFinding("missing-path: /orchestrator-home/.agent/cat.json\nmissing-path: /orchestrator-home/x\n")
	if !strings.Contains(got, "/orchestrator-home/.agent/cat.json") {
		t.Fatalf("the diagnosis does not name the missing path: %q", got)
	}
	if !strings.Contains(got, "/orchestrator-home/x") {
		t.Errorf("the diagnosis names only the first missing path: %q", got)
	}
	// It must say WHOSE fault the class is, because the remedy is not in the
	// box: the path comes from the provider's own arguments.
	if !strings.Contains(strings.ToLower(got), "argument") {
		t.Errorf("the diagnosis does not say the path came from the agent's arguments, so the reader looks in the image instead of the provider config: %q", got)
	}
	if empty := l.hostPathFinding("   \n"); empty != "" {
		t.Errorf("an empty report must add nothing: %q", empty)
	}
}
