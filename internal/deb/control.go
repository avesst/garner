package deb

import (
	"bytes"
	"fmt"
	"strings"
)

// Field is one field of a control paragraph. For a multi-line field, Value
// keeps the continuation lines with their leading whitespace, joined by
// "\n", so "Name: " + Value reproduces the field.
type Field struct {
	Name  string
	Value string
}

// Paragraph is a control paragraph with its fields in file order.
type Paragraph []Field

// Get returns the value of a field. Field names are case-insensitive.
func (p Paragraph) Get(name string) (string, bool) {
	for _, f := range p {
		if strings.EqualFold(f.Name, name) {
			return f.Value, true
		}
	}
	return "", false
}

// ParseParagraph parses a single deb822 paragraph, the format of a binary
// package's control file. Blank lines around it are allowed; a second
// paragraph is an error.
func ParseParagraph(data []byte) (Paragraph, error) {
	var p Paragraph
	ended := false
	for n, line := range strings.Split(string(bytes.TrimRight(data, "\n")), "\n") {
		lineNo := n + 1
		if strings.TrimSpace(line) == "" {
			ended = len(p) > 0
			continue
		}
		if ended {
			return nil, fmt.Errorf("line %d: more than one paragraph", lineNo)
		}
		if line[0] == ' ' || line[0] == '\t' {
			if len(p) == 0 {
				return nil, fmt.Errorf("line %d: continuation line before the first field", lineNo)
			}
			p[len(p)-1].Value += "\n" + strings.TrimRight(line, " \t\r")
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("line %d: missing ':' after field name", lineNo)
		}
		if !validFieldName(name) {
			return nil, fmt.Errorf("line %d: invalid field name %q", lineNo, name)
		}
		if _, dup := p.Get(name); dup {
			return nil, fmt.Errorf("line %d: duplicate field %q", lineNo, name)
		}
		p = append(p, Field{Name: name, Value: strings.TrimSpace(value)})
	}
	if len(p) == 0 {
		return nil, fmt.Errorf("empty control paragraph")
	}
	return p, nil
}

// validFieldName follows deb822: printable US-ASCII except ':' and space,
// not starting with '#' or '-'.
func validFieldName(s string) bool {
	if s == "" || s[0] == '#' || s[0] == '-' {
		return false
	}
	for _, c := range []byte(s) {
		if c <= ' ' || c > '~' || c == ':' {
			return false
		}
	}
	return true
}
