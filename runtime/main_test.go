package main

import (
	"bufio"
	"os"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"missing op", nil, exitError},
		{"protocol", []string{"protocol"}, exitOK},
		{"unknown op", []string{"peek", "s1"}, exitUnknown},
		{"unimplemented driving verb", []string{"nudge", "s1"}, exitUnknown},
		{"lifecycle op missing session name", []string{"start"}, exitError},
		{"provision missing session name", []string{"provision"}, exitError},
		{"relaunch missing session name", []string{"relaunch"}, exitError},
		{"exec missing session name", []string{"exec"}, exitError},
		{"exec missing command", []string{"exec", "s1"}, exitError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			got := run(tc.args, w, w)
			w.Close()
			if got != tc.want {
				t.Fatalf("run(%v) = %d, want %d", tc.args, got, tc.want)
			}
		})
	}
}

// TestProtocolHandshakeDeclaresProvisionAndExec confirms the protocol
// handshake advertises proc.provision and proc.exec now that both are
// implemented — a provision-capable pack must also declare exec (the
// controller launches the agent over exec after provision).
func TestProtocolHandshakeDeclaresProvisionAndExec(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if got := run([]string{"protocol"}, w, w); got != exitOK {
		t.Fatalf("run(protocol) = %d, want %d", got, exitOK)
	}
	w.Close()
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil {
		t.Fatalf("reading protocol output: %v", err)
	}
	for _, capName := range []string{"proc.provision", "proc.exec"} {
		if !strings.Contains(line, capName) {
			t.Fatalf("protocol handshake %q missing capability %q", line, capName)
		}
	}
}

// TestRunLifecycleOpRequiresConfig confirms a lifecycle op fails loudly
// (not silently, not exit-2) when GC_NOMAD_ADDR/GC_NOMAD_SIDECAR_DIR are
// unset, so a misconfigured pack never masquerades as "op unimplemented".
func TestRunLifecycleOpRequiresConfig(t *testing.T) {
	t.Setenv(envAddr, "")
	t.Setenv(envSidecarDir, "")

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got := run([]string{"is-running", "s1"}, w, w)
	w.Close()
	if got != exitError {
		t.Fatalf("run(is-running) with no config = %d, want %d (exitError)", got, exitError)
	}
}
