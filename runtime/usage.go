package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// The usage surface. It exists because the first thing anyone does with a
// binary they just installed is type its name, and this one answered
// "gc-runtime-nomad: missing operation" without naming a single operation,
// while `--help` printed nothing at all and exited 0.
//
// `--help` printing nothing was not an oversight in a help path — there was
// no help path. It fell through the lifecycleOps gate, whose exit-2-and-silent
// answer is the RPP forward-compatibility contract: a newer gc probing for an
// operation this build does not implement must get "not implemented", not
// prose. That contract is correct and TestUnknownOpStaysSilentExitTwo holds it
// in place. `help`, `--help` and `-h` are not RPP operation names, so serving
// them costs the contract nothing.
//
// opHelp is keyed by operation so an operation added to lifecycleOps without a
// line here goes red in TestHelpListsEveryOperation rather than shipping
// undiscoverable.
var opHelp = map[string]string{
	"protocol":         "print the RPP handshake (capabilities this build implements) and exit",
	"check":            "report this deployment's readiness: warnings for every unset knob that changes behaviour, and a hard failure for a gc that cannot run a worker",
	"start":            "dispatch a session box and launch its agent (start config arrives on stdin)",
	"provision":        "dispatch a session box WITHOUT launching the agent",
	"relaunch":         "launch the agent in a box that already exists",
	"stop":             "stop a session's box",
	"is-running":       "exit 0 if the named session's box is running",
	"process-alive":    "exit 0 if the named process is alive inside the box",
	"list-running":     "print the session names this runtime currently holds",
	"exec":             "run a command inside the box (the command arrives on stdin, not in argv)",
	"nudge":            "send the configured nudge keystrokes to the agent",
	"peek":             "print the tail of the agent's pane",
	"interrupt":        "interrupt the agent without stopping the box",
	"send-keys":        "send literal keystrokes to the agent's pane",
	"clear-scrollback": "clear the agent pane's scrollback",
}

// usageOps is every operation this build serves, in the order a reader needs
// them: the two that answer questions about the deployment, then the lifecycle.
func usageOps() []string {
	ordered := []string{"protocol", "check", "start", "provision", "relaunch", "stop"}
	seen := map[string]bool{}
	for _, op := range ordered {
		seen[op] = true
	}
	var rest []string
	for op := range lifecycleOps {
		if !seen[op] {
			rest = append(rest, op)
		}
	}
	sort.Strings(rest)
	return append(ordered, rest...)
}

func writeUsage(w io.Writer) {
	var b strings.Builder
	b.WriteString("gc-runtime-nomad — the Nomad runtime for Gas City sessions (RPP v0).\n\n")
	b.WriteString("gc invokes this binary; you invoke it directly to check a deployment or\n")
	b.WriteString("debug a box. Configuration is entirely environment: run `check` first, which\n")
	b.WriteString("names every variable it wants and what goes wrong without it.\n\n")
	b.WriteString("usage: gc-runtime-nomad <operation> [session]\n\noperations:\n")
	for _, op := range usageOps() {
		b.WriteString(fmt.Sprintf("  %-17s %s\n", op, opHelp[op]))
	}
	b.WriteString("\nAn operation this build does not implement exits 2 and prints nothing: that\n")
	b.WriteString("is the forward-compatibility answer gc reads, not an error.\n")
	b.WriteString("\nFRONT-DOOR.md is the install path; README.md is the reference; docs/ carries\n")
	b.WriteString("the pin dependencies and the debug skill.\n")
	fmt.Fprint(w, b.String())
}
