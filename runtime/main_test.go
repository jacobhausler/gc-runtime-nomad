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
		{"unknown op", []string{"start", "s1"}, exitUnknown},
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
