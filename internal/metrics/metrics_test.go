package metrics

import (
	"strings"
	"testing"
)

func TestExposition(t *testing.T) {
	r := NewRegistry()
	up := r.Counter("garner_uploads_total", "Uploads by result.", "result")
	up.Inc("ok")
	up.Inc("ok")
	up.Inc(`bad"one`)
	d := r.Gauge("garner_publish_duration_seconds", "Duration of the last publish.")
	d.Set(1.5)
	r.Collect(func(emit func(string, string, map[string]string, float64)) {
		emit("garner_packages", "Packages.", map[string]string{"format": "deb", "arch": "amd64"}, 3)
	})
	var b strings.Builder
	r.Write(&b)
	want := `# HELP garner_uploads_total Uploads by result.
# TYPE garner_uploads_total counter
garner_uploads_total{result="bad\"one"} 1
garner_uploads_total{result="ok"} 2
# HELP garner_publish_duration_seconds Duration of the last publish.
# TYPE garner_publish_duration_seconds gauge
garner_publish_duration_seconds 1.5
# HELP garner_packages Packages.
# TYPE garner_packages gauge
garner_packages{arch="amd64",format="deb"} 3
`
	if b.String() != want {
		t.Errorf("got:\n%s\nwant:\n%s", b.String(), want)
	}
}

// A collected name ending in _total is a total the database keeps, so it
// is exposed as a counter; other collected values stay gauges.
func TestCollectedTotalIsCounter(t *testing.T) {
	r := NewRegistry()
	r.Collect(func(emit func(string, string, map[string]string, float64)) {
		emit("garner_conflicts_total", "Conflicts recorded, by kind.", map[string]string{"kind": "checksum"}, 2)
		emit("garner_conflicts_open", "Open conflicts.", nil, 1)
	})
	var b strings.Builder
	r.Write(&b)
	for _, want := range []string{
		"# TYPE garner_conflicts_total counter\ngarner_conflicts_total{kind=\"checksum\"} 2\n",
		"# TYPE garner_conflicts_open gauge\ngarner_conflicts_open 1\n",
	} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("exposition lacks %q:\n%s", want, b.String())
		}
	}
}
