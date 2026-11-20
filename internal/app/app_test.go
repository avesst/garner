package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avesst/garner/internal/config"
	"github.com/avesst/garner/internal/signing"
)

// A reload applies log.format and log.level, also to loggers derived
// before it (sync derives one per source run).
func TestReloadAppliesLogSection(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "garner.yaml")
	write := func(format, level string) {
		t.Helper()
		yaml := fmt.Sprintf("server:\n  public_url: http://localhost\ndatabase:\n  url: sqlite://%s/garner.db\n"+
			"storage:\n  path: %s\nlog:\n  format: %s\n  level: %s\n", dir, dir, format, level)
		if err := os.WriteFile(cfgPath, []byte(yaml), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("json", "info")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := signing.GenerateBits(DefaultKeyPath(cfg), "t", "t@example.com", 2048); err != nil {
		t.Fatal(err)
	}

	// The logger writes to standard output.
	out, err := os.Create(filepath.Join(dir, "stdout"))
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = out
	t.Cleanup(func() { os.Stdout = stdout })

	a, err := Open(context.Background(), cfgPath, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	derived := a.Log.With("source", "s")
	grouped := derived.WithGroup("g")

	write("text", "warn")
	if err := a.Reload(); err != nil {
		t.Fatal(err)
	}
	derived.Info("hidden by the new level")
	derived.Warn("after reload")
	a.Log.Warn("base after reload")
	grouped.Warn("grouped", "k", 1)

	data, err := os.ReadFile(out.Name())
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if strings.Contains(got, "hidden by the new level") {
		t.Errorf("info record logged after reload to level warn:\n%s", got)
	}
	for _, want := range []string{`level=WARN msg="after reload" source=s`, `level=WARN msg="base after reload"`,
		`level=WARN msg=grouped source=s g.k=1`} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks text record %q:\n%s", want, got)
		}
	}
}
