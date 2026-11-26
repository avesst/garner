package deb

import (
	"fmt"
	"regexp"
	"strings"
)

// relationFields are the relationship fields of a binary package that apt
// parses into its cache, with whether dpkg allows alternatives ('|') in
// them. apt rejects the whole Packages file over one syntax error in any
// of them, so ingest checks them all.
var relationFields = []struct {
	name         string
	alternatives bool
}{
	{"Pre-Depends", true},
	{"Depends", true},
	{"Recommends", true},
	{"Suggests", true},
	{"Enhances", true},
	{"Breaks", false},
	{"Conflicts", false},
	{"Replaces", false},
	{"Provides", false},
}

// dpkg's rules for names in relationship fields, which are looser than
// those for the Package field: any case, one character and '_' are allowed.
var (
	relNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9+._-]*$`)
	relArchRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*$`)
)

// relation is one package reference in a relationship field.
type relation struct {
	Name    string
	Arch    string // qualifier after ':', "" if none
	Op      string // "<<", "<=", "=", ">=", ">>", the obsolete "<" or ">", or "" for none
	Version string
}

// relationOps are tried in order, so two-character operators match first.
var relationOps = []string{"<<", "<=", ">>", ">=", "=", "<", ">"}

// parseRelations parses a relationship field the way dpkg does: entries
// separated by ',', each a list of alternatives separated by '|', each
// "name[:arch] [(op version)]". Architecture lists and build profiles are
// for source packages only and rejected. An empty field has no entries.
func parseRelations(field string, alternatives, provides bool) ([][]relation, error) {
	s := field
	i := 0
	skip := func() {
		for i < len(s) && isSpace(rune(s[i])) {
			i++
		}
	}
	skip()
	if i == len(s) {
		return nil, nil
	}
	var out [][]relation
	for {
		var alts []relation
		for {
			skip()
			var r relation
			start := i
			for i < len(s) && !isSpace(rune(s[i])) && !strings.ContainsRune(":(,|", rune(s[i])) {
				i++
			}
			r.Name = s[start:i]
			if r.Name == "" {
				return nil, fmt.Errorf("missing package name at %q", rest(s, i))
			}
			if !relNameRE.MatchString(r.Name) {
				return nil, fmt.Errorf("invalid package name %q", r.Name)
			}
			if i < len(s) && s[i] == ':' {
				i++
				start = i
				for i < len(s) && !isSpace(rune(s[i])) && !strings.ContainsRune("(,|", rune(s[i])) {
					i++
				}
				r.Arch = s[start:i]
				if !relArchRE.MatchString(r.Arch) {
					return nil, fmt.Errorf("%s: invalid architecture qualifier %q", r.Name, r.Arch)
				}
			}
			skip()
			if i < len(s) && s[i] == '(' {
				i++
				skip()
				for _, op := range relationOps {
					if strings.HasPrefix(s[i:], op) {
						r.Op = op
						i += len(op)
						break
					}
				}
				skip()
				start = i
				for i < len(s) && !isSpace(rune(s[i])) && s[i] != ')' {
					i++
				}
				r.Version = s[start:i]
				if r.Version == "" {
					return nil, fmt.Errorf("%s: empty version", r.Name)
				}
				if _, err := ParseVersion(r.Version); err != nil {
					return nil, fmt.Errorf("%s: %w", r.Name, err)
				}
				skip()
				if i >= len(s) || s[i] != ')' {
					return nil, fmt.Errorf("%s: expected ')' after version %q, got %q", r.Name, r.Version, rest(s, i))
				}
				i++
				if provides && r.Op != "" && r.Op != "=" {
					return nil, fmt.Errorf("%s: only exact versions (=) are allowed in Provides", r.Name)
				}
			}
			alts = append(alts, r)
			skip()
			if i < len(s) && s[i] == '|' {
				if !alternatives {
					return nil, fmt.Errorf("alternatives ('|') are not allowed")
				}
				i++
				continue
			}
			break
		}
		out = append(out, alts)
		if i == len(s) {
			return out, nil
		}
		if s[i] != ',' {
			return nil, fmt.Errorf("unexpected %q after %s", rest(s, i), alts[len(alts)-1].Name)
		}
		i++
	}
}

// rest is the text from i on, shortened for error messages.
func rest(s string, i int) string {
	r := strings.TrimSpace(s[i:])
	if r == "" {
		return "end of field"
	}
	if len(r) > 20 {
		r = r[:20] + "..."
	}
	return r
}

// checkRelations parses every relationship field of a control paragraph
// and returns the lower-cased names in Replaces, which dpkg compares
// without regard to case.
func checkRelations(control Paragraph) (replaces []string, err error) {
	seen := map[string]bool{}
	for _, f := range relationFields {
		v, ok := control.Get(f.name)
		if !ok {
			continue
		}
		rels, err := parseRelations(v, f.alternatives, f.name == "Provides")
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.name, err)
		}
		if f.name != "Replaces" {
			continue
		}
		for _, alts := range rels {
			name := strings.ToLower(alts[0].Name)
			if !seen[name] {
				seen[name] = true
				replaces = append(replaces, name)
			}
		}
	}
	return replaces, nil
}
