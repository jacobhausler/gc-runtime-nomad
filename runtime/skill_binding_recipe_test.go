package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The skill's own text says the sidecar binding is "the only layer that can
// tell two faults apart", and an outside SRE review found that every decisive
// claim in the document routed through evidence it never said how to read. Two
// people (one of them me, twice) have already concluded "the bindings are
// empty" from grepping the wrong key names.
//
// So the recipe is held to the CODE. A runbook that names the wrong JSON keys
// is worse than one that names none: it produces a confident wrong conclusion.

func skillBody(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "skills", "gc-nomad-debug", "SKILL.md"))
	if err != nil {
		t.Fatalf("reading SKILL.md: %v", err)
	}
	return string(b)
}

// TestSkillNamesTheRealBindingKeys marshals a real binding and requires the
// skill to name the keys that actually appear.
func TestSkillNamesTheRealBindingKeys(t *testing.T) {
	raw, err := json.Marshal(binding{SessionName: "s", ChildJobID: "c", StagePending: true})
	if err != nil {
		t.Fatalf("marshalling a binding: %v", err)
	}
	var asMap map[string]any
	if err := json.Unmarshal(raw, &asMap); err != nil {
		t.Fatalf("unmarshalling: %v", err)
	}
	doc := skillBody(t)
	for _, key := range []string{"child_job_id", "stage_pending"} {
		if _, ok := asMap[key]; !ok {
			t.Fatalf("the binding no longer serialises %q; the skill's recipe is now wrong", key)
		}
		if !strings.Contains(doc, key) {
			t.Errorf("SKILL.md does not name the on-disk key %q. Grepping for a CamelCase field "+
				"name returns nothing and reads exactly like \"the bindings are empty\", which is "+
				"the wrong conclusion and has been reached twice.", key)
		}
	}
	// And it must not print the Go field names as if they were the on-disk
	// keys, which is what it used to do.
	for _, camel := range []string{"`ChildJobID`", "`StagePending`"} {
		if strings.Contains(doc, camel) {
			t.Errorf("SKILL.md still presents %s as a key to look for; that is the Go field name, "+
				"not what is in the file", camel)
		}
	}
}

// TestSkillFilenameDerivationMatchesTheCode pins the scheme. base64url with
// padding STRIPPED is what the code does (RawURLEncoding); a recipe using
// padded base64 produces a filename that does not exist.
func TestSkillFilenameDerivationMatchesTheCode(t *testing.T) {
	const session = "worker-local-1-pool"
	want := base64.RawURLEncoding.EncodeToString([]byte(session)) + ".json"

	s := &sidecar{dir: "/tmp/x"}
	got := filepath.Base(s.path(session))
	if got != want {
		t.Fatalf("binding filename = %q, want %q — the skill's recipe is derived from this", got, want)
	}
	if strings.Contains(want, "=") {
		t.Fatal("RawURLEncoding should never pad; the recipe strips padding on that basis")
	}

	doc := skillBody(t)
	if !strings.Contains(doc, "base64url") {
		t.Error("SKILL.md does not say the filename is base64url of the session name, so an `ls` " +
			"shows opaque names and tells the reader nothing")
	}
	if !strings.Contains(doc, "basenc") && !strings.Contains(doc, "urlsafe_b64encode") {
		t.Error("SKILL.md gives no copy-pasteable way to derive the filename; naming the scheme " +
			"without a command is what made this section unexecutable at 3am")
	}
}

// TestSkillDoesNotOverpromiseTheCheckFailure guards a claim I got wrong.
// `check` FAILS on an observed refusal and WARNS when it cannot determine the
// capability; the skill said it "FAILS on this now", so a reader on an
// unreachable cluster would get a warning, expect a FAIL, and discard a correct
// diagnosis.
func TestSkillDoesNotOverpromiseTheCheckFailure(t *testing.T) {
	doc := skillBody(t)
	if strings.Contains(doc, "it FAILS on this now") {
		t.Error("SKILL.md claims check FAILS unconditionally on the credential case; it warns when " +
			"it cannot determine the capability, and that warning is not an all-clear")
	}
	if !strings.Contains(doc, "WARNS instead") {
		t.Error("SKILL.md does not tell the reader to distinguish the FAIL from the WARN")
	}
}

// TestSkillExecRecipeMatchesTheCallingConvention pins a defect this repo
// shipped in the skill for about an hour.
//
// `exec` reads the command from STDIN (main.go, the exec case); argv after the
// session name is ignored. The skill's recipe was written as
// `exec <session> ps -eo pid,comm`, which does not run anything — on a
// terminal it blocks reading stdin, which reads exactly like an unreachable
// box. An outside SRE working a staged 3am page called it a blocker and said
// they would have escalated on a false premise.
func TestSkillExecRecipeMatchesTheCallingConvention(t *testing.T) {
	src := readSourceFile(t, "main.go")
	execCase := src[strings.Index(src, `case "exec":`):]
	if end := strings.Index(execCase, "\n\tcase "); end > 0 {
		execCase = execCase[:end]
	}
	if !strings.Contains(execCase, "io.ReadAll(stdin)") {
		t.Fatal("exec no longer reads its command from stdin; the skill's recipe is now wrong " +
			"in the other direction")
	}

	doc := skillBody(t)
	// The recipe must PIPE the command in.
	if !strings.Contains(doc, "| gc-runtime-nomad exec") {
		t.Error("SKILL.md does not show the command being piped into exec. Written as argv it runs " +
			"nothing and blocks on a tty, which is indistinguishable from an unreachable box.")
	}
	// And it must warn, because the argv spelling is the one people reach for.
	if !strings.Contains(doc, "not argv") {
		t.Error("SKILL.md does not warn that argv after the session name is ignored")
	}
}

// TestCheckPrintsDiagnosticsPathBeforeFailing guards an ordering that was
// circular.
//
// The skill says the only sanctioned way to learn GC_NOMAD_SIDECAR_DIR is the
// `diagnostics: <path>` line `check` prints, and then sends the reader to
// diagnostics.log on the credential-failure row. The allocation-read probe used
// to run BEFORE that line, so a FAIL returned without ever printing the address
// of the evidence about the failure.
func TestCheckPrintsDiagnosticsPathBeforeFailing(t *testing.T) {
	src := readSourceFile(t, "main.go")
	diag := strings.Index(src, `fmt.Fprintf(stderr, "diagnostics: %s\n", path)`)
	probe := strings.Index(src, "checkAllocListCapability(stderr)")
	if diag < 0 || probe < 0 {
		t.Fatal("could not locate both the diagnostics print and the capability probe")
	}
	if probe < diag {
		t.Error("the allocation-read probe runs BEFORE check prints the diagnostics path, so a FAIL " +
			"suppresses the only documented way to find diagnostics.log — and the shipped skill " +
			"sends the reader there for exactly this failure")
	}
}
