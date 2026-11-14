package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "no command", args: nil, wantCode: 2, wantStderr: "Usage: garner"},
		{name: "version", args: []string{"version"}, wantCode: 0, wantStdout: "garner "},
		{name: "version flag", args: []string{"--version"}, wantCode: 0, wantStdout: "garner "},
		{name: "help", args: []string{"help"}, wantCode: 0, wantStdout: "Usage: garner"},
		{name: "unknown command", args: []string{"frobnicate"}, wantCode: 2, wantStderr: `unknown command "frobnicate"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(tt.args, &stdout, &stderr)
			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d", code, tt.wantCode)
			}
			if !strings.Contains(stdout.String(), tt.wantStdout) {
				t.Errorf("stdout = %q, want it to contain %q", stdout.String(), tt.wantStdout)
			}
			if !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.wantStderr)
			}
		})
	}
}

func TestVersionStringPrefersLinkTimeVersion(t *testing.T) {
	old := version
	t.Cleanup(func() { version = old })
	version = "1.2.3"
	if got := versionString(); got != "1.2.3" {
		t.Errorf("versionString() = %q, want %q", got, "1.2.3")
	}
}
