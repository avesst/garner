package main

import (
	"bytes"
	"io"
	"slices"
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
		{name: "config without subcommand", args: []string{"config"}, wantCode: 2, wantStderr: "Usage: garner config validate"},
		{name: "valid config", args: []string{"config", "validate", "../../internal/config/testdata/design-example.yaml"}, wantCode: 0, wantStdout: "valid (sha256 "},
		{name: "missing config", args: []string{"config", "validate", "/nonexistent/garner.yaml"}, wantCode: 1, wantStderr: "no such file"},
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

func TestParseInterspersed(t *testing.T) {
	tests := []struct {
		args      []string
		wantPos   []string
		wantSuite string
	}{
		{[]string{"a.deb", "--suite", "noble", "b.deb"}, []string{"a.deb", "b.deb"}, "noble"},
		{[]string{"--suite=noble", "a.deb"}, []string{"a.deb"}, "noble"},
		{[]string{"a.deb", "--", "--suite"}, []string{"a.deb", "--suite"}, ""},
		{nil, nil, ""},
	}
	for _, tt := range tests {
		fs := newFlags("test", io.Discard)
		suite := fs.String("suite", "", "")
		pos, err := parse(fs, tt.args)
		if err != nil {
			t.Fatalf("%v: %v", tt.args, err)
		}
		if !slices.Equal(pos, tt.wantPos) || *suite != tt.wantSuite {
			t.Errorf("%v: pos %q suite %q, want %q %q", tt.args, pos, *suite, tt.wantPos, tt.wantSuite)
		}
	}
}
