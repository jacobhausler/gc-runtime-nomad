package main

import (
	"os"
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
		{"unknown op", []string{"exec", "s1"}, exitUnknown},
		{"unimplemented driving verb", []string{"nudge", "s1"}, exitUnknown},
		{"lifecycle op missing session name", []string{"start"}, exitError},
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
