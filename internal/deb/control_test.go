package deb

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// A control file is up to maxControlFile bytes from an untrusted upload,
// so parsing one must take time linear in its size.
func TestParseParagraphManyFields(t *testing.T) {
	var b strings.Builder
	b.WriteString("Package: x\n")
	for i := 0; b.Len() < maxControlFile-32; i++ {
		fmt.Fprintf(&b, "X%d: v\n", i)
	}
	start := time.Now()
	p, err := ParseParagraph([]byte(b.String()))
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("parsing %d fields took %v", len(p), d)
	}
	b.WriteString("x1: again\n")
	if _, err := ParseParagraph([]byte(b.String())); err == nil || !strings.Contains(err.Error(), `duplicate field "x1"`) {
		t.Errorf("err = %v, want a duplicate field, compared case-insensitively", err)
	}
}

func TestParseParagraphLongMultilineField(t *testing.T) {
	var b strings.Builder
	b.WriteString("Package: x\nDescription: d\n")
	lines := 0
	for ; b.Len() < maxControlFile-8; lines++ {
		b.WriteString(" a\n")
	}
	start := time.Now()
	p, err := ParseParagraph([]byte(b.String()))
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("parsing a field of %d lines took %v", lines, d)
	}
	if v, _ := p.Get("Description"); v != "d"+strings.Repeat("\n a", lines) {
		t.Errorf("Description has %d bytes, want %d", len(v), 1+3*lines)
	}
}
