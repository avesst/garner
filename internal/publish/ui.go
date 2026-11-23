package publish

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/avesst/garner/internal/config"
	"github.com/avesst/garner/internal/format"
	"github.com/avesst/garner/internal/fsutil"
	"github.com/avesst/garner/internal/signing"
	"github.com/avesst/garner/internal/store"
)

type clientSection struct {
	Format string
	// Label and Tool name the format for people ("Debian / Ubuntu", "apt").
	Label, Tool string
	// Key names the distribution in this format ("suite", "release").
	Key      string
	Variants []clientVariant
}

type clientVariant struct {
	Distribution string
	FileName     string
	Path         string
	File         string
	Instructions string
}

// countGrid is one format's package counts, distributions by architecture.
type countGrid struct {
	Format string
	Archs  []string
	Rows   []gridRow
}

type gridRow struct {
	Distribution string
	Cells        []gridCell
}

type gridCell struct {
	Count int
	// Empty marks a configured distribution and architecture with no
	// packages, which usually means a build is missing.
	Empty bool
	// Alpha shades the cell by its count relative to the largest one.
	Alpha string
}

// ecosystem is one tab of the UI: the enabled formats of one
// namespace (section 14).
type ecosystem struct {
	// ID is the namespace.
	ID      string
	Label   string
	Signed  bool
	Formats []format.Format
	// Kind is the kind of sync source that delivers the ecosystem,
	// "forge" or "registry", as the status API reports it.
	Kind string
}

// ecosystems groups the enabled formats by namespace, in registration
// order.
func (p *Publisher) ecosystems() []*ecosystem {
	var out []*ecosystem
	byNS := map[string]*ecosystem{}
	for _, fm := range p.Formats.All() {
		e, ok := byNS[fm.Namespace()]
		if !ok {
			e = &ecosystem{ID: fm.Namespace(), Label: fm.Namespace(), Kind: "forge"}
			if pr, ok := fm.(format.Presenter); ok {
				if pres := pr.Presentation(); pres.Ecosystem != "" {
					e.Label = pres.Ecosystem
				}
			}
			byNS[e.ID] = e
			out = append(out, e)
		}
		if pr, ok := fm.(format.Presenter); ok && pr.Presentation().Signed {
			e.Signed = true
		}
		if _, ok := fm.(format.RegistryFormat); ok {
			e.Kind = "registry"
		}
		e.Formats = append(e.Formats, fm)
	}
	return out
}

// presentation returns a format's presentation, defaulting to its name.
func presentation(fm format.Format) format.Presentation {
	if pr, ok := fm.(format.Presenter); ok {
		pres := pr.Presentation()
		if pres.Label == "" {
			pres.Label = fm.Name()
		}
		return pres
	}
	return format.Presentation{Label: fm.Name()}
}

// ecoTab is an ecosystem's tab in the UI.
type ecoTab struct {
	*ecosystem
	Clients   []clientSection
	Grids     []countGrid
	Total     int // packages counted in Grids
	Summaries []format.Summary
	// Rows list every package name of the tab, paged and filtered by the
	// page's script.
	Rows []format.Row
}

type uiData struct {
	Name         string
	URL          string
	Fingerprint  []string // in groups of four, as gpg prints it
	Tabs         []ecoTab
	PublishedAt  string
	SnapshotID   string
	ConfigHash   string
	ConfigCommit string
}

// writeRootFiles writes the UI, the keyring
// and the client files into the snapshot, so they change with it
// (section 4).
func (p *Publisher) writeRootFiles(dir string, cfg *config.Config, signer signing.Signer,
	snap *store.Snapshot, pkgs []*format.Package, tags []format.Tag) error {
	armored, binary, err := signer.PublicKeys()
	if err != nil {
		return err
	}
	if err := fsutil.WriteFile(filepath.Join(dir, "key.asc"), armored, 0o644); err != nil {
		return err
	}
	if err := fsutil.WriteFile(filepath.Join(dir, "key.gpg"), binary, 0o644); err != nil {
		return err
	}
	data := uiData{
		Name:        cfg.Server.ClientName,
		URL:         publicURL(cfg),
		Fingerprint: groups(signer.Fingerprint(), 4),
		PublishedAt: time.Now().UTC().Format(time.RFC3339),
		SnapshotID:  snap.ID,
		ConfigHash:  cfg.Hash,
	}
	if p.ConfigCommit != nil {
		data.ConfigCommit = p.ConfigCommit()
	}
	byFormat := map[string][]*format.Package{}
	for _, pk := range pkgs {
		byFormat[pk.Format] = append(byFormat[pk.Format], pk)
	}
	tagsByFormat := map[string][]format.Tag{}
	for _, t := range tags {
		tagsByFormat[t.Format] = append(tagsByFormat[t.Format], t)
	}
	for _, e := range p.ecosystems() {
		tab := ecoTab{ecosystem: e}
		var rows []format.Row
		var plain []format.Format
		for _, fm := range e.Formats {
			own, ownTags := byFormat[fm.Name()], tagsByFormat[fm.Name()]
			if l, ok := fm.(format.Lister); ok {
				tab.Summaries = append(tab.Summaries, l.Summary(cfg, data.URL, own, ownTags))
				rows = append(rows, l.Rows(cfg, data.URL, own, ownTags)...)
				continue
			}
			plain = append(plain, fm)
			pres := presentation(fm)
			sec := clientSection{Format: fm.Name(), Label: pres.Label, Tool: pres.Tool, Key: fm.DistributionKey()}
			for _, cf := range fm.Clients(cfg, data.URL) {
				if err := fsutil.WriteFile(filepath.Join(dir, filepath.FromSlash(cf.Path)), cf.Content, 0o644); err != nil {
					return err
				}
				sec.Variants = append(sec.Variants, clientVariant{
					Distribution: cf.Distribution, FileName: cf.Name, Path: cf.Path,
					File: string(cf.Content), Instructions: cf.Instructions,
				})
			}
			if len(sec.Variants) > 0 {
				tab.Clients = append(tab.Clients, sec)
			}
		}
		if len(plain) > 0 {
			tab.Grids, tab.Total = grids(plain, cfg, pkgs)
			rows = append(rows, packageRows(plain, pkgs)...)
		}
		tab.Rows = sortRows(rows)
		data.Tabs = append(data.Tabs, tab)
	}
	tmpl, err := p.ui()
	if err != nil {
		return err
	}
	var b bytes.Buffer
	if err := tmpl.Execute(&b, data); err != nil {
		return err
	}
	return fsutil.WriteFile(filepath.Join(dir, "index.html"), b.Bytes(), 0o644)
}

// sortRows orders rows by group, then keeps their order within it, rows
// without a group first.
func sortRows(rows []format.Row) []format.Row {
	slices.SortStableFunc(rows, func(a, b format.Row) int { return strings.Compare(a.Group, b.Group) })
	return rows
}

// packageRows lists every package name of formats that do not list
// themselves: per format and distribution, its versions, newest first,
// with their architectures.
func packageRows(fms []format.Format, pkgs []*format.Package) []format.Row {
	listed := map[string]bool{}
	for _, fm := range fms {
		listed[fm.Name()] = true
	}
	byName := map[string][]*format.Package{}
	for _, pk := range pkgs {
		if listed[pk.Format] {
			byName[pk.Name] = append(byName[pk.Name], pk)
		}
	}
	var names []string
	for n := range byName {
		names = append(names, n)
	}
	slices.Sort(names)
	var rows []format.Row
	for _, n := range names {
		row := format.Row{Name: n}
		for _, fm := range fms {
			var own []*format.Package
			for _, pk := range byName[n] {
				if pk.Format == fm.Name() {
					own = append(own, pk)
				}
			}
			if len(own) == 0 {
				continue
			}
			label := presentation(fm).Label
			dists := map[string][]*format.Package{}
			var order []string
			for _, pk := range own {
				if _, ok := dists[pk.Distribution]; !ok {
					order = append(order, pk.Distribution)
				}
				dists[pk.Distribution] = append(dists[pk.Distribution], pk)
			}
			slices.Sort(order)
			for _, d := range order {
				ps := dists[d]
				slices.SortStableFunc(ps, func(a, b *format.Package) int { return fm.Compare(b, a) })
				var versions []string
				archs := map[string][]string{}
				for _, pk := range ps {
					v := fullVersion(pk)
					if _, ok := archs[v]; !ok {
						versions = append(versions, v)
					}
					if pk.Arch != "" && !slices.Contains(archs[v], pk.Arch) {
						archs[v] = append(archs[v], pk.Arch)
					}
				}
				parts := make([]string, len(versions))
				for i, v := range versions {
					parts[i] = v
					if a := archs[v]; len(a) > 0 {
						slices.Sort(a)
						parts[i] += " (" + strings.Join(a, ", ") + ")"
					}
				}
				row.Facts = append(row.Facts, format.Fact{Label: label + " · " + d, Value: strings.Join(parts, ", ")})
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// fullVersion formats epoch, version and release in the usual way.
func fullVersion(p *format.Package) string {
	v := p.Version
	if p.Release != "" {
		v += "-" + p.Release
	}
	if p.Epoch != 0 {
		v = fmt.Sprintf("%d:%s", p.Epoch, v)
	}
	return v
}

func groups(s string, n int) []string {
	var out []string
	for len(s) > n {
		out = append(out, s[:n])
		s = s[n:]
	}
	return append(out, s)
}

// grids lays out package counts per format: the configured distributions
// and architectures, so a configured cell without packages shows up, then
// the architecture-independent column when it has packages. Distributions
// and architectures that are no longer configured but still hold packages
// are appended rather than hidden.
func grids(fms []format.Format, cfg *config.Config, pkgs []*format.Package) ([]countGrid, int) {
	var out []countGrid
	total, most := 0, 0
	for _, fm := range fms {
		dists := slices.Clone(fm.Distributions(cfg))
		archs := slices.Clone(fm.Architectures(cfg))
		confDists, confArchs := len(dists), len(archs)
		indep := fm.ArchIndependent()
		n := map[[2]string]int{}
		hasIndep := false
		var extra []string
		for _, pk := range pkgs {
			if pk.Format != fm.Name() {
				continue
			}
			n[[2]string{pk.Distribution, pk.Arch}]++
			total++
			if !slices.Contains(dists, pk.Distribution) {
				dists = append(dists, pk.Distribution)
			}
			switch {
			case pk.Arch == indep:
				hasIndep = true
			case !slices.Contains(archs, pk.Arch) && !slices.Contains(extra, pk.Arch):
				extra = append(extra, pk.Arch)
			}
		}
		if len(dists) == 0 {
			continue
		}
		slices.Sort(extra)
		archs = append(archs, extra...)
		if hasIndep {
			archs = append(archs, indep)
		}
		g := countGrid{Format: fm.Name(), Archs: archs}
		for di, d := range dists {
			r := gridRow{Distribution: d}
			for ai, a := range archs {
				c := gridCell{Count: n[[2]string{d, a}]}
				c.Empty = c.Count == 0 && di < confDists && ai < confArchs
				most = max(most, c.Count)
				r.Cells = append(r.Cells, c)
			}
			g.Rows = append(g.Rows, r)
		}
		out = append(out, g)
	}
	for _, g := range out {
		for _, r := range g.Rows {
			for i := range r.Cells {
				if c := &r.Cells[i]; c.Count > 0 {
					c.Alpha = fmt.Sprintf("%.2f", 0.12+0.55*float64(c.Count)/float64(most))
				}
			}
		}
	}
	return out, total
}

//go:embed ui.html
var uiFS embed.FS

var uiTemplate = template.Must(parseUI(uiFS))

func parseUI(fsys fs.FS) (*template.Template, error) {
	return template.New("ui.html").Funcs(template.FuncMap{"lower": strings.ToLower}).ParseFS(fsys, "ui.html")
}

// ui returns the page template: the embedded one, or with UIFS
// set, one parsed from it now, so a preview shows edits without a rebuild.
func (p *Publisher) ui() (*template.Template, error) {
	if p.UIFS == nil {
		return uiTemplate, nil
	}
	return parseUI(p.UIFS)
}
