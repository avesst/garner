package publish

import (
	"bytes"
	"fmt"
	"html/template"
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

// ecosystem is one tab of the landing page: the enabled formats of one
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

// ecoTab is an ecosystem's tab on the landing page.
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

type landingData struct {
	Name         string
	URL          string
	Fingerprint  []string // in groups of four, as gpg prints it
	Tabs         []ecoTab
	PublishedAt  string
	SnapshotID   string
	ConfigHash   string
	ConfigCommit string
}

// writeRootFiles writes the landing page, the keyring
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
	data := landingData{
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
	var b bytes.Buffer
	if err := landingTemplate.Execute(&b, data); err != nil {
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

var landingTemplate = template.Must(template.New("index").Funcs(template.FuncMap{"lower": strings.ToLower}).Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Name}} repository</title>
<style>
:root {
  --bg: #f8f8f5; --panel: #ffffff; --fg: #1c1e1b; --muted: #676b63; --line: #dfe1da;
  --code: #f1f2ed; --accent: #2f6f4f; --accent-soft: #e3efe8; --heat: 47, 111, 79;
  --ok: #2f7a4a; --bad: #b3261e; --bad-soft: #fbe9e7; --warn: #8a5a00; --warn-soft: #f8eed8;
  --sans: system-ui, -apple-system, "Segoe UI", Roboto, "Noto Sans", sans-serif;
  --mono: ui-monospace, "SFMono-Regular", "DejaVu Sans Mono", Menlo, Consolas, monospace;
}
@media (prefers-color-scheme: dark) {
  :root { --bg: #151614; --panel: #1c1d1a; --fg: #e7e8e3; --muted: #9c9f96; --line: #31332e;
    --code: #22231f; --accent: #7cc19a; --accent-soft: #1f3328; --heat: 124, 193, 154;
    --ok: #7cc19a; --bad: #f2867a; --bad-soft: #3a1f1c; --warn: #e3b55c; --warn-soft: #352a14; color-scheme: dark; }
}
* { box-sizing: border-box; }
/* Components below set display, which would otherwise override hidden. */
[hidden] { display: none !important; }
body { margin: 0; background: var(--bg); color: var(--fg); font: 16px/1.5 var(--sans); }
code, pre { font-family: var(--mono); }
button { font: inherit; }
a { color: var(--accent); }
:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }
main { max-width: 54rem; margin: 0 auto; padding: 2.5rem 1rem 4rem; display: grid; gap: 2.25rem; }
main > * { min-width: 0; }
header { display: grid; gap: .5rem; }
.eyebrow { font-size: .75rem; letter-spacing: .08em; text-transform: uppercase; color: var(--muted); margin: 0; }
h1 { font-size: 2rem; line-height: 1.15; margin: 0; letter-spacing: -.01em; overflow-wrap: anywhere; }
.pills { display: flex; flex-wrap: wrap; gap: .5rem; margin-top: .25rem; }
.pill { display: inline-flex; align-items: center; gap: .4rem; font-size: .8125rem; padding: .2rem .65rem;
  border-radius: 999px; border: 1px solid var(--line); background: var(--panel); }
.pill i { width: 7px; height: 7px; border-radius: 50%; background: var(--muted); flex: none; }
.pill.ok i { background: var(--ok); }
.pill.bad { border-color: transparent; background: var(--bad-soft); color: var(--bad); }
.pill.bad i { background: var(--bad); }
.pill.warn { border-color: transparent; background: var(--warn-soft); color: var(--warn); }
.pill.warn i { background: var(--warn); }
.pill a { color: inherit; }
h2 { font-size: 1.05rem; margin: 0 0 .75rem; }
h3, h4 { font-size: 1rem; margin: 0; }
.install { background: var(--panel); border: 1px solid var(--line); border-radius: 10px; overflow: hidden; }
.tabs { display: flex; border-bottom: 1px solid var(--line); overflow-x: auto; }
.tabs button { border: 0; background: none; color: var(--muted); padding: .75rem 1.1rem; cursor: pointer;
  border-bottom: 2px solid transparent; white-space: nowrap; text-align: left; }
.tabs small, .fmt-h small { display: block; font: .75rem var(--mono); color: var(--muted); }
.tabs button[aria-selected="true"] { color: var(--fg); border-bottom-color: var(--accent); font-weight: 600; }
.tabs button[aria-selected="true"] small { font-weight: 400; }
.fmt { padding: 1rem 1.1rem 1.1rem; display: grid; gap: .75rem; }
.fmt + .fmt { border-top: 1px solid var(--line); }
.picker { display: flex; align-items: center; gap: .4rem; flex-wrap: wrap; font-size: .8125rem; color: var(--muted); }
.picker button { border: 1px solid var(--line); background: none; color: var(--fg); border-radius: 6px;
  padding: .1rem .6rem; cursor: pointer; font: .8125rem var(--mono); }
.picker button[aria-pressed="true"] { background: var(--accent-soft); border-color: var(--accent); color: var(--accent); }
.variant { display: grid; gap: .75rem; }
.fmt > *, .variant > * { min-width: 0; }
.code { position: relative; }
pre { margin: 0; background: var(--code); border-radius: 6px; padding: .75rem 3rem .75rem .9rem;
  overflow-x: auto; font-size: .84rem; line-height: 1.6; }
.copy { position: absolute; top: .4rem; right: .4rem; padding: .3rem; line-height: 0; color: var(--muted);
  background: var(--panel); border: 1px solid var(--line); border-radius: 5px; cursor: pointer; opacity: 0; transition: opacity .15s; }
.copy svg { width: 16px; height: 16px; fill: none; stroke: currentColor; stroke-width: 2; stroke-linecap: round; stroke-linejoin: round; }
.copy:hover { color: var(--fg); }
/* Shown on hover, on keyboard focus, while reporting a result, and always
   on devices without hover. */
.code:hover .copy, .copy:focus-visible, .copy.done { opacity: 1; }
@media (hover: none) { .copy { opacity: 1; } }
details { font-size: .875rem; }
summary { cursor: pointer; color: var(--muted); }
details .code, details > pre { margin-top: .5rem; }
.two { display: grid; grid-template-columns: minmax(0, 1.25fr) minmax(0, 1fr); gap: 2.25rem; }
.two > * { min-width: 0; }
.stack { display: grid; gap: 2rem; align-content: start; }
@media (max-width: 720px) { .two { grid-template-columns: minmax(0, 1fr); } }
.matrix + .matrix { margin-top: 1.1rem; }
.matrix h3 { font-size: .8125rem; margin: 0 0 .25rem; color: var(--muted); }
.mx { overflow-x: auto; }
.mx table { border-collapse: separate; border-spacing: 3px; font-size: .875rem; margin-left: -3px; }
.mx th { font-weight: 500; color: var(--muted); font-size: .75rem; padding: .1rem .5rem; text-align: left; white-space: nowrap; }
.mx thead th { font-family: var(--mono); text-align: center; }
.mx tbody th { font: .8125rem var(--mono); color: var(--fg); padding-left: 0; }
.mx td { min-width: 4.25rem; text-align: center; padding: .45rem .6rem; border-radius: 4px;
  font-variant-numeric: tabular-nums; font-weight: 600; }
.mx td.empty { color: var(--muted); font-weight: 400; outline: 1px dashed var(--line); outline-offset: -1px; }
.mx td.none { color: var(--muted); font-weight: 400; }
.note { font-size: .8125rem; color: var(--muted); margin: .6rem 0 0; }
.fp { font: .875rem var(--mono); display: grid; grid-template-columns: repeat(5, max-content); gap: .1rem .75rem; margin: 0 0 .6rem; }
.links { display: flex; gap: .9rem; font-size: .875rem; }
.src { display: grid; gap: .6rem; font-size: .875rem; }
.src > div { display: grid; grid-template-columns: 8px minmax(0, 1fr); gap: .1rem .6rem; align-items: baseline; }
.src i { width: 8px; height: 8px; border-radius: 50%; background: var(--ok); }
.src .bad i { background: var(--bad); }
.src .sub { grid-column: 2; color: var(--muted); font-size: .8125rem; overflow-wrap: anywhere; }
.src .bad .sub { color: var(--bad); }
.src summary { font-size: .875rem; }
.src .more.bad summary { color: var(--bad); }
.src .list { max-height: 20rem; overflow-y: auto; margin-top: .6rem; padding-right: .25rem; }
.muted { color: var(--muted); font-size: .875rem; margin: 0; }
dl { display: grid; grid-template-columns: max-content minmax(0, 1fr); gap: .35rem 1rem; font-size: .875rem; margin: 0; }
dt { color: var(--muted); }
dd { margin: 0; overflow-wrap: anywhere; font: .8125rem var(--mono); }
@media (prefers-reduced-motion: reduce) { .copy { transition: none; } }
.eco { display: grid; gap: 1.75rem; }
.eco > h2.eco-h { font-size: 1.35rem; margin: 0; }
.eco-tabs { display: flex; gap: .25rem; border-bottom: 1px solid var(--line); overflow-x: auto; margin-bottom: 1.75rem; }
.eco-tabs button { border: 0; background: none; color: var(--muted); padding: .6rem 1rem; cursor: pointer;
  border-bottom: 2px solid transparent; white-space: nowrap; font-size: 1rem; }
.eco-tabs button[aria-selected="true"] { color: var(--fg); border-bottom-color: var(--accent); font-weight: 600; }
.stats { display: flex; flex-wrap: wrap; gap: .5rem 1.5rem; margin: 0; font-size: .9375rem; }
.stats strong { font-size: 1.35rem; font-variant-numeric: tabular-nums; margin-right: .25rem; }
.groups { display: flex; flex-wrap: wrap; gap: .4rem; margin: .6rem 0 0; font-size: .8125rem; }
.groups span { border: 1px solid var(--line); border-radius: 999px; padding: .1rem .6rem; background: var(--panel); }
.groups code { font-size: .8125rem; }
.list h2 .count { font-weight: 400; }
.tools { display: flex; flex-wrap: wrap; align-items: center; gap: .5rem 1rem; margin-bottom: .5rem; }
.filter { width: 100%; max-width: 24rem; font: inherit; padding: .4rem .6rem; border: 1px solid var(--line);
  border-radius: 6px; background: var(--panel); color: var(--fg); }
.pager { display: flex; align-items: center; gap: .5rem; font-size: .875rem; color: var(--muted); }
.pager button { border: 1px solid var(--line); background: var(--panel); color: var(--fg); border-radius: 6px;
  padding: .2rem .7rem; cursor: pointer; }
.pager button:disabled { opacity: .4; cursor: default; }
.rows { list-style: none; margin: 0; padding: 0; background: var(--panel); border: 1px solid var(--line);
  border-radius: 10px; overflow: hidden; }
.row { display: grid; gap: .4rem; padding: .75rem 1rem; border-top: 1px solid var(--line); }
.row:first-child, .row.first { border-top: 0; }
.row:hover { background: var(--code); }
.row .head { display: flex; flex-wrap: wrap; align-items: center; gap: .35rem .6rem; min-width: 0; }
.row .name { font: 600 .9375rem var(--mono); overflow-wrap: anywhere; }
.row .tags { display: flex; flex-wrap: wrap; gap: .3rem; align-items: center; font-size: .8125rem; }
.row .tags code { font-size: .75rem; padding: .05rem .45rem; border-radius: 999px; background: var(--accent-soft);
  color: var(--accent); }
.row .tags a { color: var(--muted); }
.copy-row { margin-left: auto; display: inline-flex; align-items: center; gap: .35rem; font: .75rem var(--mono);
  color: var(--muted); background: var(--panel); border: 1px solid var(--line); border-radius: 6px;
  padding: .15rem .5rem; cursor: pointer; }
.copy-row:hover, .copy-row.done { color: var(--fg); border-color: var(--accent); }
.copy-row svg { width: 13px; height: 13px; fill: none; stroke: currentColor; stroke-width: 2; stroke-linecap: round; stroke-linejoin: round; }
.facts { display: flex; flex-wrap: wrap; gap: .25rem 1.25rem; margin: 0; font-size: .8125rem; }
.facts div { display: flex; gap: .4rem; min-width: 0; }
.facts dt { color: var(--muted); }
.facts dd { margin: 0; font: .8125rem var(--mono); overflow-wrap: anywhere; }
footer { border-top: 1px solid var(--line); padding-top: 1rem; font-size: .8125rem; color: var(--muted); }
footer p { margin: 0; }
footer code { font: .75rem var(--mono); color: var(--fg); }
</style>
</head>
<body>
<main>
<header>
<p class="eyebrow">Repository</p>
<h1>{{.Name}}</h1>
<div class="pills" id="pills"><span class="pill"><i></i><span>Published <time id="published" datetime="{{.PublishedAt}}">{{.PublishedAt}}</time></span></span></div>
</header>

<div id="ecos">
{{- range .Tabs}}
<section class="eco" id="eco-{{.ID}}" data-kind="{{.Kind}}">
<h2 class="eco-h">{{.Label}}</h2>
{{- if .Clients}}
<section>
<h2>Add this repository</h2>
<div class="install">
{{- range .Clients}}
<section class="fmt" data-key="{{.Key}}">
<h3 class="fmt-h">{{.Label}}{{if .Tool}} <small>{{.Tool}}</small>{{end}}</h3>
{{- $multi := gt (len .Variants) 1}}
{{- range $i, $v := .Variants}}
<div class="variant" data-dist="{{.Distribution}}">
{{- if $multi}}
<h4>{{.Distribution}}{{if eq $i 0}} (default){{end}}</h4>
{{- end}}
<pre><code>{{.Instructions}}</code></pre>
<details><summary>What this installs: <a href="{{.Path}}">{{.FileName}}</a></summary>
<pre><code>{{.File}}</code></pre>
</details>
</div>
{{- end}}
</section>
{{- end}}
</div>
</section>
{{- end}}
{{- range .Summaries}}
<section>
<h2>Use it</h2>
<pre><code>{{.Usage}}</code></pre>
</section>
{{- end}}

<div class="two">
<section>
<h2>Contents</h2>
{{- range .Summaries}}
<p class="stats">{{range .Stats}}<span><strong>{{.Count}}</strong>{{.Label}}</span>{{end}}</p>
{{- if .Groups}}
<div class="groups">{{range .Groups}}<span><code>{{.Label}}</code> {{.Count}}</span>{{end}}{{if .More}}<span>and {{.More}} more</span>{{end}}</div>
{{- end}}
{{- end}}
{{- range .Grids}}
<div class="matrix">
<h3>{{.Format}}</h3>
<div class="mx"><table>
<thead><tr><th></th>{{range .Archs}}<th scope="col">{{.}}</th>{{end}}</tr></thead>
<tbody>
{{- range .Rows}}
<tr><th scope="row">{{.Distribution}}</th>
{{- range .Cells}}{{if .Empty}}<td class="empty">0</td>{{else if eq .Count 0}}<td class="none">0</td>{{else}}<td style="background: rgba(var(--heat), {{.Alpha}})">{{.Count}}</td>{{end}}{{end}}</tr>
{{- end}}
</tbody>
</table></div>
</div>
{{- end}}
{{- if .Grids}}
<p class="note">{{if .Total}}{{.Total}} {{if eq .Total 1}}package{{else}}packages{{end}} in this snapshot.{{else}}No packages yet.{{end}}
A dashed cell is a configured architecture with nothing published.</p>
{{- end}}
</section>

<div class="stack">
{{- if .Signed}}
<section>
<h2>Signing key</h2>
<p class="fp">{{range $i, $g := $.Fingerprint}}{{if $i}} {{end}}<span>{{$g}}</span>{{end}}</p>
<div class="links"><a href="key.asc">key.asc</a><a href="key.gpg">key.gpg</a></div>
</section>
{{- end}}
<section>
<h2>Sync sources</h2>
<div class="src" data-kind="{{.Kind}}"><p class="muted">Loading live status…</p></div>
</section>
</div>
</div>

<section class="list">
<h2>Browse <span class="muted count">({{len .Rows}})</span></h2>
{{- if not .Rows}}
<p class="muted">Nothing published yet.</p>
{{- else}}
<ul class="rows">
{{- range .Rows}}
<li class="row" data-name="{{.Name}}">
<div class="head"><strong class="name">{{.Name}}</strong>
{{- if .Tags}}
<span class="tags">{{range .Tags}}<code>{{.}}</code>{{end}}{{if .MoreTags}}<a href="{{.TagsURL}}">+{{.MoreTags}} more</a>{{end}}</span>
{{- end}}
{{- if .Copy}}
<button type="button" class="copy-row" data-copy="{{.Copy}}" aria-label="{{.CopyLabel}}" title="{{.Copy}}" hidden>pull</button>
{{- end}}</div>
{{- if .Facts}}
<dl class="facts">{{range .Facts}}<div><dt>{{.Label}}</dt><dd>{{.Value}}</dd></div>{{end}}</dl>
{{- end}}
</li>
{{- end}}
</ul>
{{- end}}
</section>
</section>
{{- end}}
</div>

<footer>
<p>Snapshot <code>{{.SnapshotID}}</code>, published <time datetime="{{.PublishedAt}}">{{.PublishedAt}}</time>,
configuration <code>{{.ConfigHash}}</code>{{if .ConfigCommit}} (commit <code>{{.ConfigCommit}}</code>){{end}}</p>
</footer>
</main>
<script>
function ago(iso) {
  var secs = (Date.now() - Date.parse(iso)) / 1000;
  if (!(secs >= 0)) return iso;
  return secs < 60 ? "just now" : secs < 3600 ? Math.floor(secs / 60) + " min ago" :
    secs < 172800 ? Math.floor(secs / 3600) + " h ago" : Math.floor(secs / 86400) + " days ago";
}

function tabBar(cls, sections, heading, container, before) {
  var tabs = document.createElement("div");
  tabs.className = cls;
  tabs.setAttribute("role", "tablist");
  var tbs = [];
  sections.forEach(function (f, i) {
    var h = f.querySelector(heading);
    var b = document.createElement("button");
    b.type = "button";
    b.setAttribute("role", "tab");
    b.innerHTML = h.innerHTML;
    h.hidden = true;
    b.addEventListener("click", function () {
      sections.forEach(function (x, j) { x.hidden = i !== j; });
      tbs.forEach(function (x, j) { x.setAttribute("aria-selected", String(i === j)); });
      if (cls === "eco-tabs") try { history.replaceState(null, "", "#" + f.id); } catch (e) {}
    });
    tbs.push(b);
    tabs.appendChild(b);
  });
  container.insertBefore(tabs, before);
  return tbs;
}

(function () {
  // Without script every ecosystem, format and variant is shown under its
  // own heading; with it, tabs pick the ecosystem and the format, and a
  // picker the variant.
  var ecos = Array.prototype.slice.call(document.querySelectorAll("section.eco"));
  if (ecos.length > 1) {
    var tbs = tabBar("eco-tabs", ecos, ".eco-h", document.getElementById("ecos"), ecos[0]);
    var pick = ecos.findIndex(function (e) { return "#" + e.id === location.hash; });
    tbs[pick < 0 ? 0 : pick].click();
  } else if (ecos.length === 1) {
    ecos[0].querySelector(".eco-h").hidden = true;
  }
  document.querySelectorAll(".install").forEach(function (install) {
    var fmts = Array.prototype.slice.call(install.querySelectorAll("section.fmt"));
    if (fmts.length > 1) tabBar("tabs", fmts, ".fmt-h", install, fmts[0])[0].click();
    fmts.forEach(function (sec) {
      var variants = sec.querySelectorAll(".variant");
      if (variants.length < 2) return;
      var key = sec.getAttribute("data-key");
      var picker = document.createElement("div");
      picker.className = "picker";
      picker.setAttribute("role", "group");
      picker.setAttribute("aria-label", key);
      var label = document.createElement("span");
      label.textContent = key.charAt(0).toUpperCase() + key.slice(1) + ":";
      picker.appendChild(label);
      var buttons = [];
      function show(i) {
        variants.forEach(function (v, j) { v.hidden = i !== j; });
        buttons.forEach(function (b, j) { b.setAttribute("aria-pressed", String(i === j)); });
      }
      variants.forEach(function (v, i) {
        v.querySelector("h4").hidden = true;
        var b = document.createElement("button");
        b.type = "button";
        b.textContent = v.getAttribute("data-dist");
        b.addEventListener("click", function () { show(i); });
        buttons.push(b);
        picker.appendChild(b);
      });
      sec.insertBefore(picker, variants[0]);
      show(0);
    });
  });

  // navigator.clipboard needs a secure context; repositories are often
  // served over plain HTTP, so fall back to execCommand there.
  function copy(s) {
    if (navigator.clipboard && window.isSecureContext) return navigator.clipboard.writeText(s);
    return new Promise(function (ok, fail) {
      var t = document.createElement("textarea");
      t.value = s;
      t.setAttribute("readonly", "");
      t.style.position = "fixed";
      t.style.opacity = "0";
      document.body.appendChild(t);
      t.select();
      var done = false;
      try { done = document.execCommand("copy"); } catch (e) {}
      document.body.removeChild(t);
      if (done) ok(); else fail();
    });
  }
  var icons = {
    Copy: '<rect x="9" y="9" width="12" height="12" rx="2"/><path d="M5 15H4a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1h10a1 1 0 0 1 1 1v1"/>',
    Copied: '<path d="M4 12l5 5L20 6"/>',
    "Copy failed": '<path d="M6 6l12 12M18 6L6 18"/>'
  };
  function setIcon(b, label) {
    b.innerHTML = '<svg viewBox="0 0 24 24" aria-hidden="true">' + icons[label] + "</svg>";
    b.setAttribute("aria-label", label);
    b.title = label;
  }
  document.querySelectorAll("main pre").forEach(function (pre) {
    var wrap = document.createElement("div");
    wrap.className = "code";
    pre.parentNode.insertBefore(wrap, pre);
    wrap.appendChild(pre);
    var b = document.createElement("button");
    b.type = "button";
    b.className = "copy";
    setIcon(b, "Copy");
    var timer;
    b.addEventListener("click", function () {
      function show(label) {
        setIcon(b, label);
        b.classList.add("done");
        clearTimeout(timer);
        timer = setTimeout(function () { setIcon(b, "Copy"); b.classList.remove("done"); }, 1500);
      }
      copy(pre.textContent).then(function () { show("Copied"); }, function () { show("Copy failed"); });
    });
    wrap.appendChild(b);
  });

  // Rows copy their command without printing it; without script the
  // buttons stay hidden.
  document.querySelectorAll(".copy-row").forEach(function (b) {
    var label = b.textContent, timer;
    function show(icon, text) {
      b.innerHTML = '<svg viewBox="0 0 24 24" aria-hidden="true">' + icons[icon] + "</svg><span>" + text + "</span>";
    }
    show("Copy", label);
    b.hidden = false;
    b.addEventListener("click", function () {
      function done(icon, text) {
        show(icon, text);
        b.classList.add("done");
        clearTimeout(timer);
        timer = setTimeout(function () { show("Copy", label); b.classList.remove("done"); }, 1500);
      }
      copy(b.getAttribute("data-copy")).then(function () { done("Copied", "copied"); },
        function () { done("Copy failed", "failed"); });
    });
  });

  // Relative publish time; the exact time stays in the tooltip and below.
  var pub = document.getElementById("published");
  pub.title = pub.textContent;
  pub.textContent = ago(pub.getAttribute("datetime"));
})();

(function () {
  var pills = document.getElementById("pills");
  var boxes = document.querySelectorAll(".src[data-kind]");
  function text(tag, s, cls) { var e = document.createElement(tag); if (s != null) e.textContent = s; if (cls) e.className = cls; return e; }
  function pill(cls, s) { var p = text("span", null, "pill " + cls); p.appendChild(text("i")); p.appendChild(document.createTextNode(s)); pills.appendChild(p); return p; }
  // The same page is served at the root, current/ and snapshots/<id>/;
  // find the repository root from wherever it was loaded.
  var path = location.pathname;
  var pinned = /\/snapshots\/[^\/]+\/(index\.html)?$/.test(path);
  var base = path.replace(/[^\/]*$/, "").replace(/(current|snapshots\/[^\/]+)\/$/, "");
  fetch(base + "api/v1/status", { cache: "no-store" }).then(function (r) {
    if (!r.ok) throw new Error(r.status);
    return r.json();
  }).then(function (s) {
    if (s.current_snapshot && s.current_snapshot !== "{{.SnapshotID}}") {
      if (pinned) {
        // Snapshot pages never change, so reloading would not help.
        var p = pill("warn", "Snapshot {{.SnapshotID}}; the current one is " + s.current_snapshot + ". ");
        var a = text("a", "Open the current page");
        a.href = base;
        p.appendChild(a);
      } else {
        pill("warn", "A newer snapshot is published; reload the page");
      }
    }
    var all = s.sources || [];
    var failing = all.filter(function (x) { return x.last_error; }).length;
    if (failing) pill("bad", failing + " of " + all.length + " sources failing");
    else if (all.length) pill("ok", all.length === 1 ? "Source syncing" : "All " + all.length + " sources syncing");
    if (s.open_conflicts) pill("warn", s.open_conflicts + (s.open_conflicts === 1 ? " open conflict" : " open conflicts"));
    function row(x) {
      var d = text("div", null, x.last_error ? "bad" : "");
      d.appendChild(text("i"));
      d.appendChild(text("span", x.name));
      d.appendChild(text("span", x.last_error || (x.last_success ? "Last success " + x.last_success : "Not synced yet"), "sub"));
      return d;
    }
    function more(label, list, cls) {
      var d = text("details", null, "more " + cls);
      d.appendChild(text("summary", label));
      var box = text("div", null, "src list");
      list.forEach(function (x) { box.appendChild(row(x)); });
      d.appendChild(box);
      return d;
    }
    // Each tab lists the sources of its kind. Failing sources first. With
    // many sources, the healthy ones collapse into one line that names the
    // oldest success, so a source that stopped syncing without an error
    // still stands out; expanded, they are listed oldest first.
    boxes.forEach(function (el) {
      el.textContent = "";
      var kind = el.getAttribute("data-kind");
      var srcs = all.filter(function (x) { return (x.kind || "forge") === kind; });
      if (srcs.length === 0) { el.appendChild(text("p", "No sync sources for these.", "muted")); return; }
      var bad = srcs.filter(function (x) { return x.last_error; });
      var good = srcs.filter(function (x) { return !x.last_error; });
      good.sort(function (a, b) { return (Date.parse(a.last_success) || 0) - (Date.parse(b.last_success) || 0); });
      if (srcs.length <= 5) {
        bad.concat(good).forEach(function (x) { el.appendChild(row(x)); });
        return;
      }
      bad.slice(0, 10).forEach(function (x) { el.appendChild(row(x)); });
      if (bad.length > 10) el.appendChild(more("and " + (bad.length - 10) + " more failing", bad.slice(10), "bad"));
      if (good.length) {
        var synced = good.filter(function (x) { return x.last_success; });
        var label = good.length + (good.length === 1 ? " source" : " sources") + " syncing";
        if (synced.length) label += ", oldest success " + ago(synced[0].last_success);
        if (synced.length < good.length) label += ", " + (good.length - synced.length) + " not synced yet";
        el.appendChild(more(label, good, ""));
      }
    });
  }).catch(function () {
    pill("warn", "Live status unavailable");
    boxes.forEach(function (el) {
      el.textContent = "";
      el.appendChild(text("p", "garner is not reachable. The repository itself still works.", "muted"));
    });
  });
})();
(function () {
  // Without script every list shows in full. With it, a box filters a
  // list by name and the matches show a page at a time.
  var pageSize = 25;
  document.querySelectorAll("section.list").forEach(function (list) {
    var rows = list.querySelector(".rows");
    if (!rows) return;
    var items = Array.prototype.slice.call(rows.querySelectorAll(".row"));
    var count = list.querySelector(".count");
    var tools = document.createElement("div");
    tools.className = "tools";
    var input = document.createElement("input");
    input.type = "search";
    input.className = "filter";
    input.placeholder = "Filter by name";
    input.setAttribute("aria-label", "Filter by name");
    tools.appendChild(input);
    var pager = document.createElement("div");
    pager.className = "pager";
    var prev = document.createElement("button");
    prev.type = "button";
    prev.textContent = "Previous";
    var label = document.createElement("span");
    label.setAttribute("aria-live", "polite");
    var next = document.createElement("button");
    next.type = "button";
    next.textContent = "Next";
    pager.appendChild(prev);
    pager.appendChild(label);
    pager.appendChild(next);
    tools.appendChild(pager);
    rows.parentNode.insertBefore(tools, rows);
    var none = document.createElement("p");
    none.className = "muted";
    none.textContent = "No names match.";
    none.hidden = true;
    rows.parentNode.insertBefore(none, rows.nextSibling);
    var page = 0, matches = items;
    function render() {
      var pages = Math.max(1, Math.ceil(matches.length / pageSize));
      page = Math.min(page, pages - 1);
      var from = page * pageSize, to = Math.min(from + pageSize, matches.length);
      items.forEach(function (r) { r.hidden = true; r.classList.remove("first"); });
      for (var i = from; i < to; i++) matches[i].hidden = false;
      if (from < to) matches[from].classList.add("first");
      label.textContent = matches.length ? (from + 1) + "–" + to + " of " + matches.length : "no matches";
      prev.disabled = page === 0;
      next.disabled = page >= pages - 1;
      // The heading's count says how many match; the pager only matters
      // with more than one page.
      pager.hidden = pages <= 1;
      rows.hidden = matches.length === 0;
      none.hidden = matches.length > 0;
      count.textContent = "(" + (matches.length === items.length ? items.length : matches.length + " of " + items.length) + ")";
    }
    input.addEventListener("input", function () {
      var q = input.value.trim().toLowerCase();
      matches = items.filter(function (r) { return !q || r.getAttribute("data-name").toLowerCase().indexOf(q) >= 0; });
      page = 0;
      render();
    });
    prev.addEventListener("click", function () { page--; render(); });
    next.addEventListener("click", function () { page++; render(); });
    render();
  });
})();
</script>
</body>
</html>
`))
