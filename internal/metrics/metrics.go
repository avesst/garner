// Package metrics is a small Prometheus text-format registry (design
// section 13): counters and gauges with labels, plus gauges computed at
// scrape time. It avoids a dependency on the Prometheus client library for
// the handful of metrics garner exports.
package metrics

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
)

type kind string

const (
	counter kind = "counter"
	gauge   kind = "gauge"
)

// Vec is a family of counters or gauges with the same label names.
type Vec struct {
	name, help string
	kind       kind
	labels     []string

	mu     sync.Mutex
	values map[string]float64 // key: label values joined by \xff
}

// Registry holds metric families in registration order.
type Registry struct {
	mu        sync.Mutex
	vecs      []*Vec
	collector []func(emit func(name, help string, labels map[string]string, value float64))
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{} }

func (r *Registry) add(name, help string, k kind, labels []string) *Vec {
	v := &Vec{name: name, help: help, kind: k, labels: labels, values: map[string]float64{}}
	r.mu.Lock()
	r.vecs = append(r.vecs, v)
	r.mu.Unlock()
	return v
}

// Counter registers a counter family.
func (r *Registry) Counter(name, help string, labels ...string) *Vec {
	return r.add(name, help, counter, labels)
}

// Gauge registers a gauge family.
func (r *Registry) Gauge(name, help string, labels ...string) *Vec {
	return r.add(name, help, gauge, labels)
}

// Collect registers a function that emits values at scrape time, for
// values read from the database or disk. They are gauges, except names
// ending in _total, which are counters by Prometheus convention: totals
// the database keeps. Each name must be emitted by one collector only.
func (r *Registry) Collect(fn func(emit func(name, help string, labels map[string]string, value float64))) {
	r.mu.Lock()
	r.collector = append(r.collector, fn)
	r.mu.Unlock()
}

func (v *Vec) key(values []string) string {
	if len(values) != len(v.labels) {
		panic(fmt.Sprintf("metric %s: %d label values, want %d", v.name, len(values), len(v.labels)))
	}
	return strings.Join(values, "\xff")
}

// Add adds delta to the series with the given label values.
func (v *Vec) Add(delta float64, values ...string) {
	k := v.key(values)
	v.mu.Lock()
	v.values[k] += delta
	v.mu.Unlock()
}

// Inc adds one.
func (v *Vec) Inc(values ...string) { v.Add(1, values...) }

// Set sets a gauge.
func (v *Vec) Set(value float64, values ...string) {
	k := v.key(values)
	v.mu.Lock()
	v.values[k] = value
	v.mu.Unlock()
}

// Value returns the current value of a series, for tests.
func (v *Vec) Value(values ...string) float64 {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.values[v.key(values)]
}

// Write writes every family in the Prometheus text format.
func (r *Registry) Write(w io.Writer) {
	r.mu.Lock()
	vecs := slices.Clone(r.vecs)
	collectors := slices.Clone(r.collector)
	r.mu.Unlock()
	for _, v := range vecs {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", v.name, v.help, v.name, v.kind)
		v.mu.Lock()
		keys := make([]string, 0, len(v.values))
		for k := range v.values {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			var vals []string
			if len(v.labels) > 0 {
				vals = strings.Split(k, "\xff")
			}
			fmt.Fprintf(w, "%s%s %s\n", v.name, labelString(v.labels, vals), formatValue(v.values[k]))
		}
		v.mu.Unlock()
	}
	type series struct {
		labels map[string]string
		value  float64
	}
	families := map[string][]series{}
	helps := map[string]string{}
	var order []string
	for _, c := range collectors {
		c(func(name, help string, labels map[string]string, value float64) {
			if _, ok := helps[name]; !ok {
				helps[name] = help
				order = append(order, name)
			}
			families[name] = append(families[name], series{labels, value})
		})
	}
	for _, name := range order {
		typ := "gauge"
		if strings.HasSuffix(name, "_total") {
			typ = "counter"
		}
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, helps[name], name, typ)
		for _, s := range families[name] {
			names := make([]string, 0, len(s.labels))
			for n := range s.labels {
				names = append(names, n)
			}
			slices.Sort(names)
			vals := make([]string, len(names))
			for i, n := range names {
				vals[i] = s.labels[n]
			}
			fmt.Fprintf(w, "%s%s %s\n", name, labelString(names, vals), formatValue(s.value))
		}
	}
}

func labelString(names, values []string) string {
	if len(names) == 0 {
		return ""
	}
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = n + `="` + escape(values[i]) + `"`
	}
	return "{" + strings.Join(parts, ",") + "}"
}

var escaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func escape(s string) string { return escaper.Replace(s) }

func formatValue(f float64) string {
	switch {
	case math.IsInf(f, 1):
		return "+Inf"
	case math.IsInf(f, -1):
		return "-Inf"
	case math.IsNaN(f):
		return "NaN"
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// Handler serves the registry.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		r.Write(w)
	})
}
