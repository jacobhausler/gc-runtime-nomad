package main

import (
	"strings"
	"testing"
)

// The 2026-09-12 fleet outage, reproduced as a fixture. A pin roll installed
// 75fbc2cf5b, the swap door skipped its artifact publish on the way out of an
// inconclusive readiness probe, and every box then fetched a URL derived from
// the new commit and took a 404 before its task started. What the operator saw
// was `the "agent" task never started (state "pending")`, which pointed into
// the box — the one place where nothing was wrong.
const (
	gcArtifactURL    = "https://github.com/jacobhausler/gascity/releases/download/gc-75fbc2cf5b/gc-linux-amd64"
	imageTarURL      = "https://build.example/gc-agent-noble.tar"
	fetch404Event    = `Failed Artifact Download: failed to download artifact "` + gcArtifactURL + `": unexpected response code: 404`
	fetchChecksum    = `Failed Artifact Download: failed to download artifact "` + gcArtifactURL + `": Checksums did not match for /local/gcbin/gc`
	imageTarEvent    = `Failed Artifact Download: failed to download artifact "` + imageTarURL + `": dial tcp: connection refused`
	unknownURLEvent  = `Failed Artifact Download: failed to download artifact "https://elsewhere.example/thing.bin": 403 Forbidden`
	pullDeniedEvent  = `Failed to pull "ghcr.io/x/y:z": pull access denied`
	oomEvent         = `Terminated: OOM Killed`
	neverStartsEvent = `Task hook failed: prestart: some other thing`
)

func known() knownArtifacts {
	return knownArtifacts{gc: gcArtifactURL, imageTar: imageTarURL}
}

func alloc(event string) []allocRecord {
	return []allocRecord{{
		ID: "a1", ClientStatus: "failed",
		TaskStates: map[string]allocTaskState{"agent": {State: "pending", FailureEvent: event}},
	}}
}

func TestArtifactFetchFailureNamesTheCauseAndWhoseFixItIs(t *testing.T) {
	cases := []struct {
		name  string
		event string
		want  []string
		deny  []string
		why   string
	}{
		{
			name:  "a 404 on the derived gc URL sends the operator to the publishing side",
			event: fetch404Event,
			want: []string{
				"never ran",
				"FETCH fault, not a start fault",
				gcArtifactURL,
				"NOT CONFIGURED ANYWHERE",
				"PUBLISHING side",
				envGCArtifactTemplate,
			},
			deny: []string{"never started"},
			why: "the URL is a function of the live pin, so there is no jobspec to fix and no box to " +
				"look inside — saying the task 'never started' is what sent the operator into the box",
		},
		{
			name:  "a checksum mismatch is NOT reported as a missing publish",
			event: fetchChecksum,
			want:  []string{"bytes ARRIVED and failed verification", "Republish BOTH"},
			deny:  []string{"PUBLISHING side, for this exact commit"},
			why: "the bytes are there; one of the artifact and its sidecar was republished without " +
				"the other, and 'publish it' would have them republish what already exists",
		},
		{
			name:  "the image tarball is the operator's own URL, and says so",
			event: imageTarEvent,
			want:  []string{imageTarURL, envAgentImageTar, "IS yours to fix"},
			deny:  []string{"NOT CONFIGURED ANYWHERE"},
			why:   "the two URLs have opposite remedies, so conflating them is worse than a generic message",
		},
		{
			name:  "a URL this pack never declared gets the honest non-answer",
			event: unknownURLEvent,
			want:  []string{"did not declare that URL", "elsewhere.example"},
			deny:  []string{"PUBLISHING side", "IS yours to fix"},
			why:   "guessing the gc remedy for an unrelated artifact sends the operator to publish the wrong thing",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := allocFailureDiagnosis("ghcr.io/x/agent:1", alloc(tc.event), "agent", known())
			if got == "" {
				t.Fatalf("no diagnosis at all — %s", tc.why)
			}
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("diagnosis does not contain %q — %s\ngot: %s", w, tc.why, got)
				}
			}
			for _, d := range tc.deny {
				if strings.Contains(got, d) {
					t.Errorf("diagnosis still contains %q — %s\ngot: %s", d, tc.why, got)
				}
			}
		})
	}
}

// The ordering claim, stated as a test because it is not obvious from reading
// the cases: artifacts are staged before the driver is asked for the image, so
// an allocation that died fetching cannot have failed a pull or been OOM-killed,
// and an event carrying both vocabularies is a fetch fault.
func TestArtifactFetchOutranksPullAndOOM(t *testing.T) {
	both := fetch404Event + " " + pullDeniedEvent + " " + oomEvent
	got := allocFailureDiagnosis("ghcr.io/x/agent:1", alloc(both), "agent", known())
	if !strings.Contains(got, "FETCH fault") {
		t.Fatalf("an event naming a failed fetch AND a denied pull AND an OOM must be diagnosed as the fetch:\ngot: %s", got)
	}
	if strings.Contains(got, "OOM-killed") || strings.Contains(got, "is not on this node") {
		t.Fatalf("the fetch fault was overtaken by a later cause:\ngot: %s", got)
	}
}

// And the causes that are NOT artifact faults must not be captured by it — a
// cause that matches everything names nothing.
func TestNonFetchFailuresKeepTheirOwnCauses(t *testing.T) {
	for _, tc := range []struct{ name, event, want string }{
		{"a denied pull is still a pull", pullDeniedEvent, "is not on this node"},
		{"an OOM is still an OOM", oomEvent, "OOM-killed"},
		{"anything else is still the generic start fault", neverStartsEvent, "never started"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := allocFailureDiagnosis("ghcr.io/x/agent:1", alloc(tc.event), "agent", known())
			if !strings.Contains(got, tc.want) {
				t.Fatalf("diagnosis does not contain %q:\ngot: %s", tc.want, got)
			}
			if strings.Contains(got, "FETCH fault") {
				t.Fatalf("a non-fetch failure was diagnosed as a fetch fault:\ngot: %s", got)
			}
		})
	}
}

// An unconfigured artifact must not match an empty substring. strings.Contains
// is true for "" against every event, so a pack with the gc download switched
// off would otherwise report every unrelated fetch failure as the derived gc
// URL — and the empty-string case is exactly the default.
func TestUnconfiguredArtifactsDoNotClaimEveryFailure(t *testing.T) {
	got := allocFailureDiagnosis("ghcr.io/x/agent:1", alloc(unknownURLEvent), "agent", knownArtifacts{})
	if strings.Contains(got, "NOT CONFIGURED ANYWHERE") || strings.Contains(got, "IS yours to fix") {
		t.Fatalf("an empty known-artifact URL matched an unrelated event:\ngot: %s", got)
	}
	if !strings.Contains(got, "did not declare that URL") {
		t.Fatalf("want the honest non-answer:\ngot: %s", got)
	}
}
