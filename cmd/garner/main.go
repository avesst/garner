// Command garner serves an internal deb and rpm package repository.
package main

import (
	"fmt"
	"io"
	"os"
	"runtime/debug"

	"github.com/avesst/garner/internal/config"
)

// version is set at build time with -ldflags "-X main.version=...".
var version string

const usage = `Usage: garner <command> [arguments]

Commands:
  config validate [FILE]   Check a configuration file (default ` + config.DefaultPath + `).
  version                  Print the version and exit.
  help                     Show this help.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run executes one command and returns the process exit code. It takes its
// output streams as arguments so tests can call it directly.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "config":
		return runConfig(args[1:], stdout, stderr)
	case "version", "--version":
		fmt.Fprintln(stdout, "garner", versionString())
		return 0
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "garner: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

func runConfig(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "validate" || len(args) > 2 {
		fmt.Fprintf(stderr, "Usage: garner config validate [FILE]\n")
		return 2
	}
	path := config.DefaultPath
	if len(args) == 2 {
		path = args[1]
	}
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", path, err)
		return 1
	}
	fmt.Fprintf(stdout, "%s: valid (sha256 %s)\n", path, cfg.Hash[:12])
	return 0
}

// versionString prefers the version set at link time, then the module
// version, which go build derives from git (a pseudo-version between tags).
func versionString() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		if v := info.Main.Version; v != "" && v != "(devel)" {
			return v
		}
	}
	return "devel"
}
