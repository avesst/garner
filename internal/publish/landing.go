package publish

import (
	"bytes"
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

type countRow struct {
	Format, Distribution, Arch string
	Count                      int
}

type clientSection struct {
	Format       string
	FileName     string
	File         string
	Instructions string
}

type landingData struct {
	Name         string
	URL          string
	Fingerprint  string
	Clients      []clientSection
	Counts       []countRow
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
		Fingerprint: signer.Fingerprint(),
		PublishedAt: time.Now().UTC().Format(time.RFC3339),
		SnapshotID:  snap.ID,
		ConfigHash:  cfg.Hash,
	}
	if p.ConfigCommit != nil {
		data.ConfigCommit = p.ConfigCommit()
	}
	for _, fm := range p.Formats.All() {
		cf := fm.Client(cfg, data.URL)
		if err := fsutil.WriteFile(filepath.Join(dir, cf.Name), cf.Content, 0o644); err != nil {
			return err
		}
		data.Clients = append(data.Clients, clientSection{
			Format: fm.Name(), FileName: cf.Name, File: string(cf.Content), Instructions: cf.Instructions,
		})
	}
	data.Counts = counts(pkgs)
	var b bytes.Buffer
	if err := landingTemplate.Execute(&b, data); err != nil {
		return err
	}
	return fsutil.WriteFile(filepath.Join(dir, "index.html"), b.Bytes(), 0o644)
}

func counts(pkgs []*format.Package) []countRow {
	m := map[countRow]int{}
	for _, p := range pkgs {
		m[countRow{Format: p.Format, Distribution: p.Distribution, Arch: p.Arch}]++
	}
	var out []countRow
	for k, n := range m {
		k.Count = n
		out = append(out, k)
	}
	slices.SortFunc(out, func(a, b countRow) int {
		return strings.Compare(a.Format+"\x00"+a.Distribution+"\x00"+a.Arch,
			b.Format+"\x00"+b.Distribution+"\x00"+b.Arch)
	})
	return out
}

var landingTemplate = template.Must(template.New("index").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Name}} package repository</title>
<style>
:root { --bg: #fafaf8; --fg: #1d1d1b; --muted: #6b6b66; --line: #ddddd6; --code: #f0f0eb; --accent: #2f6f4f; }
@media (prefers-color-scheme: dark) {
  :root { --bg: #161615; --fg: #e8e8e3; --muted: #9a9a93; --line: #33332f; --code: #22221f; --accent: #7cc19a; }
}
* { box-sizing: border-box; }
body { margin: 0; background: var(--bg); color: var(--fg); font: 16px/1.5 system-ui, sans-serif; }
main { max-width: 52rem; margin: 0 auto; padding: 2rem 1rem 4rem; }
h1 { font-size: 1.6rem; margin: 0 0 .25rem; }
h2 { font-size: 1.15rem; margin: 2rem 0 .5rem; border-bottom: 1px solid var(--line); padding-bottom: .25rem; }
p.lead { color: var(--muted); margin: 0 0 1rem; }
pre { background: var(--code); padding: .75rem 1rem; overflow-x: auto; border-radius: 4px; font-size: .875rem; }
code { font-family: ui-monospace, monospace; }
table { border-collapse: collapse; width: 100%; font-size: .9rem; }
th, td { text-align: left; padding: .3rem .5rem; border-bottom: 1px solid var(--line); }
td.n { text-align: right; font-variant-numeric: tabular-nums; }
dl { display: grid; grid-template-columns: max-content 1fr; gap: .25rem 1rem; font-size: .9rem; }
dt { color: var(--muted); }
dd { margin: 0; overflow-wrap: anywhere; }
a { color: var(--accent); }
#status-body { font-size: .9rem; }
.err { color: #b3261e; }
</style>
</head>
<body>
<main>
<h1>{{.Name}}</h1>
<p class="lead">Package repository at <code>{{.URL}}</code>. Packages are signed with key
<code>{{.Fingerprint}}</code> (<a href="key.asc">key.asc</a>, <a href="key.gpg">key.gpg</a>).</p>
{{range .Clients}}
<h2>{{.Format}}</h2>
<pre><code>{{.Instructions}}</code></pre>
<p><a href="{{.FileName}}">{{.FileName}}</a>:</p>
<pre><code>{{.File}}</code></pre>
{{end}}
<h2>Packages</h2>
{{if .Counts}}<table>
<thead><tr><th>Format</th><th>Distribution</th><th>Architecture</th><th>Packages</th></tr></thead>
<tbody>{{range .Counts}}<tr><td>{{.Format}}</td><td>{{.Distribution}}</td><td>{{.Arch}}</td><td class="n">{{.Count}}</td></tr>{{end}}</tbody>
</table>{{else}}<p>No packages yet.</p>{{end}}
<h2>Snapshot</h2>
<dl>
<dt>Published</dt><dd>{{.PublishedAt}}</dd>
<dt>Snapshot</dt><dd><code>{{.SnapshotID}}</code></dd>
<dt>Configuration</dt><dd><code>{{.ConfigHash}}</code>{{if .ConfigCommit}} (commit <code>{{.ConfigCommit}}</code>){{end}}</dd>
</dl>
<h2>Status</h2>
<div id="status-body"><p>Loading live status…</p></div>
</main>
<script>
(function () {
  var el = document.getElementById("status-body");
  function text(tag, s, cls) { var e = document.createElement(tag); e.textContent = s; if (cls) e.className = cls; return e; }
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
    var srcs = s.sources || [];
    if (srcs.length === 0) el.appendChild(text("p", "No sync sources configured."));
    else {
      var t = document.createElement("table");
      t.innerHTML = "<thead><tr><th>Source</th><th>Last success</th><th>Error</th></tr></thead>";
      var tb = document.createElement("tbody");
      srcs.forEach(function (x) {
        var tr = document.createElement("tr");
        tr.appendChild(text("td", x.name));
        tr.appendChild(text("td", x.last_success || "never"));
        tr.appendChild(text("td", x.last_error || "", x.last_error ? "err" : ""));
        tb.appendChild(tr);
      });
      t.appendChild(tb);
      el.appendChild(t);
    }
    el.appendChild(text("p", "Open conflicts: " + (s.open_conflicts || 0)));
    if (s.current_snapshot && s.current_snapshot !== "{{.SnapshotID}}") {
      if (pinned) {
        // Snapshot pages never change, so reloading would not help.
        var p = text("p", "This page shows snapshot {{.SnapshotID}}. The current snapshot is " + s.current_snapshot + ": ");
        var a = text("a", "open the current page");
        a.href = base;
        p.appendChild(a);
        el.appendChild(p);
      } else {
        el.appendChild(text("p", "A newer snapshot (" + s.current_snapshot + ") has been published; reload the page."));
      }
    }
  }).catch(function () {
    el.textContent = "";
    el.appendChild(text("p", "Live status is unavailable: garner is not reachable. The repository itself still works."));
  });
})();
</script>
</body>
</html>
`))
