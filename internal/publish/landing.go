package publish

import (
	"bytes"
	"fmt"
	"html/template"
	"path/filepath"
	"slices"
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

// formatLabels describe formats on the landing page; others show their name.
var formatLabels = map[string][2]string{
	"deb": {"Debian / Ubuntu", "apt"},
	"rpm": {"Fedora / RHEL", "dnf"},
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

type landingData struct {
	Name         string
	URL          string
	Fingerprint  []string // in groups of four, as gpg prints it
	Clients      []clientSection
	Grids        []countGrid
	Total        int
	PublishedAt  string
	SnapshotID   string
	ConfigHash   string
	ConfigCommit string
}

// writeRootFiles writes the landing page, the keyring and the client files
// into the snapshot, so they change with it (section 4).
func (p *Publisher) writeRootFiles(dir string, cfg *config.Config, signer signing.Signer,
	snap *store.Snapshot, pkgs []*format.Package) error {
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
	for _, fm := range p.Formats.All() {
		sec := clientSection{Format: fm.Name(), Label: fm.Name(), Key: fm.DistributionKey()}
		if l, ok := formatLabels[fm.Name()]; ok {
			sec.Label, sec.Tool = l[0], l[1]
		}
		for _, cf := range fm.Clients(cfg, data.URL) {
			if err := fsutil.WriteFile(filepath.Join(dir, filepath.FromSlash(cf.Path)), cf.Content, 0o644); err != nil {
				return err
			}
			sec.Variants = append(sec.Variants, clientVariant{
				Distribution: cf.Distribution, FileName: cf.Name, Path: cf.Path,
				File: string(cf.Content), Instructions: cf.Instructions,
			})
		}
		data.Clients = append(data.Clients, sec)
	}
	data.Grids, data.Total = grids(p.Formats.All(), cfg, pkgs)
	var b bytes.Buffer
	if err := landingTemplate.Execute(&b, data); err != nil {
		return err
	}
	return fsutil.WriteFile(filepath.Join(dir, "index.html"), b.Bytes(), 0o644)
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

var landingTemplate = template.Must(template.New("index").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Name}} package repository</title>
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
</style>
</head>
<body>
<main>
<header>
<p class="eyebrow">Package repository</p>
<h1>{{.Name}}</h1>
<div class="pills" id="pills"><span class="pill"><i></i><span>Published <time id="published" datetime="{{.PublishedAt}}">{{.PublishedAt}}</time></span></span></div>
</header>

<section>
<h2>Add this repository</h2>
<div class="install" id="install">
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

<div class="two">
<section>
<h2>Packages</h2>
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
<p class="note">{{if .Total}}{{.Total}} {{if eq .Total 1}}package{{else}}packages{{end}} in this snapshot.{{else}}No packages yet.{{end}}
A dashed cell is a configured architecture with nothing published.</p>
</section>

<div class="stack">
<section>
<h2>Signing key</h2>
<p class="fp">{{range $i, $g := .Fingerprint}}{{if $i}} {{end}}<span>{{$g}}</span>{{end}}</p>
<div class="links"><a href="key.asc">key.asc</a><a href="key.gpg">key.gpg</a></div>
</section>
<section>
<h2>Sync sources</h2>
<div class="src" id="sources"><p class="muted">Loading live status…</p></div>
</section>
</div>
</div>

<section>
<h2>Snapshot</h2>
<dl>
<dt>Published</dt><dd>{{.PublishedAt}}</dd>
<dt>Snapshot</dt><dd>{{.SnapshotID}}</dd>
<dt>Configuration</dt><dd>{{.ConfigHash}}{{if .ConfigCommit}} (commit {{.ConfigCommit}}){{end}}</dd>
</dl>
</section>
</main>
<script>
function ago(iso) {
  var secs = (Date.now() - Date.parse(iso)) / 1000;
  if (!(secs >= 0)) return iso;
  return secs < 60 ? "just now" : secs < 3600 ? Math.floor(secs / 60) + " min ago" :
    secs < 172800 ? Math.floor(secs / 3600) + " h ago" : Math.floor(secs / 86400) + " days ago";
}

(function () {
  // Without script every format and variant is shown under its own
  // heading; with it, tabs pick the format and a picker the variant.
  var install = document.getElementById("install");
  var fmts = install.querySelectorAll("section.fmt");
  if (fmts.length > 1) {
    var tabs = document.createElement("div");
    tabs.className = "tabs";
    tabs.setAttribute("role", "tablist");
    var tbs = [];
    fmts.forEach(function (f, i) {
      var h = f.querySelector(".fmt-h");
      var b = document.createElement("button");
      b.type = "button";
      b.setAttribute("role", "tab");
      b.innerHTML = h.innerHTML;
      h.hidden = true;
      b.addEventListener("click", function () {
        fmts.forEach(function (x, j) { x.hidden = i !== j; });
        tbs.forEach(function (x, j) { x.setAttribute("aria-selected", String(i === j)); });
      });
      tbs.push(b);
      tabs.appendChild(b);
    });
    install.insertBefore(tabs, fmts[0]);
    tbs[0].click();
  }
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
  install.querySelectorAll("pre").forEach(function (pre) {
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

  // Relative publish time; the exact time stays in the tooltip and below.
  var pub = document.getElementById("published");
  pub.title = pub.textContent;
  pub.textContent = ago(pub.getAttribute("datetime"));
})();

(function () {
  var pills = document.getElementById("pills");
  var el = document.getElementById("sources");
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
    el.textContent = "";
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
    var srcs = s.sources || [];
    var failing = srcs.filter(function (x) { return x.last_error; }).length;
    if (failing) pill("bad", failing + " of " + srcs.length + " sources failing");
    else if (srcs.length) pill("ok", srcs.length === 1 ? "Source syncing" : "All " + srcs.length + " sources syncing");
    if (s.open_conflicts) pill("warn", s.open_conflicts + (s.open_conflicts === 1 ? " open conflict" : " open conflicts"));
    if (srcs.length === 0) el.appendChild(text("p", "No sync sources configured.", "muted"));
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
    // Failing sources first. With many sources, the healthy ones collapse
    // into one line that names the oldest success, so a source that stopped
    // syncing without an error still stands out; expanded, they are listed
    // oldest first.
    var bad = srcs.filter(function (x) { return x.last_error; });
    var good = srcs.filter(function (x) { return !x.last_error; });
    good.sort(function (a, b) { return (Date.parse(a.last_success) || 0) - (Date.parse(b.last_success) || 0); });
    if (srcs.length <= 5) {
      bad.concat(good).forEach(function (x) { el.appendChild(row(x)); });
    } else {
      bad.slice(0, 10).forEach(function (x) { el.appendChild(row(x)); });
      if (bad.length > 10) el.appendChild(more("and " + (bad.length - 10) + " more failing", bad.slice(10), "bad"));
      if (good.length) {
        var synced = good.filter(function (x) { return x.last_success; });
        var label = good.length + (good.length === 1 ? " source" : " sources") + " syncing";
        if (synced.length) label += ", oldest success " + ago(synced[0].last_success);
        if (synced.length < good.length) label += ", " + (good.length - synced.length) + " not synced yet";
        el.appendChild(more(label, good, ""));
      }
    }
  }).catch(function () {
    el.textContent = "";
    pill("warn", "Live status unavailable");
    el.appendChild(text("p", "garner is not reachable. The repository itself still works.", "muted"));
  });
})();
</script>
</body>
</html>
`))
