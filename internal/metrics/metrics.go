// Package metrics is a tiny Prometheus-compatible metrics registry: counters,
// gauges and histograms with labels, and the text exposition format (version
// 0.0.4) that every Prometheus-compatible scraper reads.
//
// It is hand-written on purpose. The harness avoids heavy dependencies where
// the standard library suffices, and the part of client_golang this needs
// (three metric types, text output) is a few hundred lines.
//
// Two rules keep a scrape cheap and bounded on a phone-hosted manager:
//
//   - Every labelled family has a series cap (DefaultMaxSeries). A label
//     combination beyond it is folded into one "other" series instead of
//     growing memory, so a bug or an attacker that feeds free-form text into a
//     label can cost one series, never an unbounded number. Callers must still
//     never label by workload ID, file name or any user text; the cap is the
//     safety net, not the design.
//   - Values that already live in the manager's registries (nodes by state,
//     workloads by state, store size) are not mirrored into counters: a
//     Collector computes them at scrape time from the source of truth, so they
//     cannot drift from it.
package metrics

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// DefaultMaxSeries is the most label combinations one family keeps.
const DefaultMaxSeries = 256

// OverflowLabel is the value every label takes on the folded series.
const OverflowLabel = "other"

var (
	nameRE  = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
	labelRE = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
)

// Registry holds metric families and writes them in exposition format.
type Registry struct {
	mu       sync.Mutex
	families map[string]family
}

type family interface {
	write(w *bufio.Writer)
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{families: map[string]family{}} }

func (r *Registry) add(name string, f family) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.families[name]; dup {
		panic("metrics: duplicate metric " + name)
	}
	r.families[name] = f
}

func checkNames(name string, labels []string) {
	if !nameRE.MatchString(name) {
		panic("metrics: invalid metric name " + name)
	}
	for _, l := range labels {
		if !labelRE.MatchString(l) || strings.HasPrefix(l, "__") || l == "le" {
			panic("metrics: invalid label name " + l + " on " + name)
		}
	}
}

// WriteTo writes every family, sorted by name, in text exposition format.
func (r *Registry) WriteTo(out io.Writer) (int64, error) {
	r.mu.Lock()
	names := make([]string, 0, len(r.families))
	for n := range r.families {
		names = append(names, n)
	}
	fams := make(map[string]family, len(r.families))
	for n, f := range r.families {
		fams[n] = f
	}
	r.mu.Unlock()
	sort.Strings(names)

	cw := &countWriter{w: out}
	bw := bufio.NewWriter(cw)
	for _, n := range names {
		fams[n].write(bw)
	}
	err := bw.Flush()
	return cw.n, err
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// ---- shared label/series bookkeeping

type desc struct {
	name   string
	help   string
	typ    string
	labels []string
}

func (d desc) header(w *bufio.Writer) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", d.name, escapeHelp(d.help), d.name, d.typ)
}

// series is the set of children of one labelled family, bounded.
type series[T any] struct {
	mu       sync.RWMutex
	max      int
	children map[string]*child[T]
	overflow *child[T]
	newT     func() *T
}

type child[T any] struct {
	values []string
	v      *T
}

func newSeries[T any](newT func() *T) *series[T] {
	return &series[T]{max: DefaultMaxSeries, children: map[string]*child[T]{}, newT: newT}
}

func keyOf(values []string) string { return strings.Join(values, "\xff") }

func (s *series[T]) get(nLabels int, values []string) *T {
	if len(values) != nLabels {
		panic(fmt.Sprintf("metrics: want %d label values, got %d", nLabels, len(values)))
	}
	key := keyOf(values)
	s.mu.RLock()
	c, ok := s.children[key]
	s.mu.RUnlock()
	if ok {
		return c.v
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.children[key]; ok {
		return c.v
	}
	if len(s.children) >= s.max && nLabels > 0 {
		if s.overflow == nil {
			ov := make([]string, nLabels)
			for i := range ov {
				ov[i] = OverflowLabel
			}
			s.overflow = &child[T]{values: ov, v: s.newT()}
		}
		return s.overflow.v
	}
	c = &child[T]{values: append([]string(nil), values...), v: s.newT()}
	s.children[key] = c
	return c.v
}

// snapshot returns the children sorted by label values (overflow last).
func (s *series[T]) snapshot() []*child[T] {
	s.mu.RLock()
	out := make([]*child[T], 0, len(s.children)+1)
	for _, c := range s.children {
		out = append(out, c)
	}
	ov := s.overflow
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return keyOf(out[i].values) < keyOf(out[j].values) })
	if ov != nil {
		out = append(out, ov)
	}
	return out
}

func (s *series[T]) setMax(n int) {
	s.mu.Lock()
	s.max = n
	s.mu.Unlock()
}

// ---- atomic float

type atomicFloat struct{ bits atomic.Uint64 }

func (f *atomicFloat) load() float64 { return math.Float64frombits(f.bits.Load()) }
func (f *atomicFloat) store(v float64) { f.bits.Store(math.Float64bits(v)) }
func (f *atomicFloat) add(d float64) {
	for {
		old := f.bits.Load()
		if f.bits.CompareAndSwap(old, math.Float64bits(math.Float64frombits(old)+d)) {
			return
		}
	}
}

// ---- counters

// CounterVec is a family of monotonically increasing values.
type CounterVec struct {
	d desc
	s *series[atomicFloat]
}

// Counter is one labelled child of a CounterVec.
type Counter struct{ f *atomicFloat }

// NewCounterVec registers a counter family. With no labels, use With().
func (r *Registry) NewCounterVec(name, help string, labels ...string) *CounterVec {
	checkNames(name, labels)
	c := &CounterVec{d: desc{name, help, "counter", labels}, s: newSeries(func() *atomicFloat { return new(atomicFloat) })}
	r.add(name, c)
	return c
}

// SetMaxSeries changes the series cap (before first use).
func (c *CounterVec) SetMaxSeries(n int) *CounterVec { c.s.setMax(n); return c }

// With returns the child for these label values.
func (c *CounterVec) With(values ...string) Counter {
	return Counter{c.s.get(len(c.d.labels), values)}
}

// Inc adds 1.
func (c Counter) Inc() { c.f.add(1) }

// Add adds d; a negative d is ignored (counters only go up).
func (c Counter) Add(d float64) {
	if d > 0 {
		c.f.add(d)
	}
}

// Value is the current count (for tests).
func (c Counter) Value() float64 { return c.f.load() }

func (c *CounterVec) write(w *bufio.Writer) {
	c.d.header(w)
	children := c.s.snapshot()
	if len(c.d.labels) == 0 && len(children) == 0 {
		fmt.Fprintf(w, "%s 0\n", c.d.name) // an unlabelled counter always exists
		return
	}
	for _, ch := range children {
		writeSample(w, c.d.name, c.d.labels, ch.values, "", "", ch.v.load())
	}
}

// ---- gauges

// GaugeVec is a family of values that go up and down.
type GaugeVec struct {
	d desc
	s *series[atomicFloat]
}

// Gauge is one labelled child of a GaugeVec.
type Gauge struct{ f *atomicFloat }

// NewGaugeVec registers a gauge family.
func (r *Registry) NewGaugeVec(name, help string, labels ...string) *GaugeVec {
	checkNames(name, labels)
	g := &GaugeVec{d: desc{name, help, "gauge", labels}, s: newSeries(func() *atomicFloat { return new(atomicFloat) })}
	r.add(name, g)
	return g
}

// SetMaxSeries changes the series cap (before first use).
func (g *GaugeVec) SetMaxSeries(n int) *GaugeVec { g.s.setMax(n); return g }

// With returns the child for these label values.
func (g *GaugeVec) With(values ...string) Gauge { return Gauge{g.s.get(len(g.d.labels), values)} }

// Set replaces the value.
func (g Gauge) Set(v float64) { g.f.store(v) }

// Add changes the value by d (negative allowed).
func (g Gauge) Add(d float64) { g.f.add(d) }

// Value is the current value (for tests).
func (g Gauge) Value() float64 { return g.f.load() }

func (g *GaugeVec) write(w *bufio.Writer) {
	g.d.header(w)
	children := g.s.snapshot()
	if len(g.d.labels) == 0 && len(children) == 0 {
		fmt.Fprintf(w, "%s 0\n", g.d.name)
		return
	}
	for _, ch := range children {
		writeSample(w, g.d.name, g.d.labels, ch.values, "", "", ch.v.load())
	}
}

// ---- histograms

// HistogramVec is a family of distributions with fixed buckets.
type HistogramVec struct {
	d       desc
	buckets []float64
	s       *series[histData]
}

type histData struct {
	mu     sync.Mutex
	counts []uint64 // per bucket (not cumulative); last slot is the +Inf overflow
	sum    float64
	n      uint64
}

// Histogram is one labelled child of a HistogramVec.
type Histogram struct {
	h *histData
	b []float64
}

// NewHistogramVec registers a histogram family. buckets are upper bounds in
// ascending order; +Inf is implicit.
func (r *Registry) NewHistogramVec(name, help string, buckets []float64, labels ...string) *HistogramVec {
	checkNames(name, labels)
	if !sort.Float64sAreSorted(buckets) {
		panic("metrics: buckets of " + name + " are not ascending")
	}
	b := append([]float64(nil), buckets...)
	h := &HistogramVec{d: desc{name, help, "histogram", labels}, buckets: b,
		s: newSeries(func() *histData { return &histData{counts: make([]uint64, len(b)+1)} })}
	r.add(name, h)
	return h
}

// SetMaxSeries changes the series cap (before first use).
func (h *HistogramVec) SetMaxSeries(n int) *HistogramVec { h.s.setMax(n); return h }

// With returns the child for these label values.
func (h *HistogramVec) With(values ...string) Histogram {
	return Histogram{h.s.get(len(h.d.labels), values), h.buckets}
}

// Observe records one value.
func (h Histogram) Observe(v float64) {
	if math.IsNaN(v) {
		return
	}
	i := sort.SearchFloat64s(h.b, v) // first bucket with bound >= v
	h.h.mu.Lock()
	h.h.counts[i]++
	h.h.sum += v
	h.h.n++
	h.h.mu.Unlock()
}

// Count is the number of observations (for tests).
func (h Histogram) Count() uint64 {
	h.h.mu.Lock()
	defer h.h.mu.Unlock()
	return h.h.n
}

func (h *HistogramVec) write(w *bufio.Writer) {
	h.d.header(w)
	for _, ch := range h.s.snapshot() {
		ch.v.mu.Lock()
		counts := append([]uint64(nil), ch.v.counts...)
		sum, n := ch.v.sum, ch.v.n
		ch.v.mu.Unlock()
		var cum uint64
		for i, bound := range h.buckets {
			cum += counts[i]
			writeSample(w, h.d.name+"_bucket", h.d.labels, ch.values, "le", formatFloat(bound), float64(cum))
		}
		cum += counts[len(h.buckets)]
		writeSample(w, h.d.name+"_bucket", h.d.labels, ch.values, "le", "+Inf", float64(cum))
		writeSample(w, h.d.name+"_sum", h.d.labels, ch.values, "", "", sum)
		writeSample(w, h.d.name+"_count", h.d.labels, ch.values, "", "", float64(n))
	}
}

// ---- scrape-time collectors

// Collector is a family whose samples are produced when it is scraped.
type Collector struct {
	d  desc
	fn func(emit func(value float64, labelValues ...string))
}

// NewCollector registers a family computed at scrape time: fn calls emit once
// per sample. typ is "gauge" or "counter". The caller bounds the number of
// samples fn emits; the registry does not cap them.
func (r *Registry) NewCollector(name, help, typ string, labels []string, fn func(emit func(value float64, labelValues ...string))) *Collector {
	checkNames(name, labels)
	if typ != "gauge" && typ != "counter" {
		panic("metrics: collector type must be gauge or counter")
	}
	c := &Collector{d: desc{name, help, typ, labels}, fn: fn}
	r.add(name, c)
	return c
}

type sample struct {
	v      float64
	values []string
}

func (c *Collector) write(w *bufio.Writer) {
	c.d.header(w)
	var samples []sample
	c.fn(func(v float64, values ...string) {
		if len(values) != len(c.d.labels) {
			panic(fmt.Sprintf("metrics: %s wants %d label values, got %d", c.d.name, len(c.d.labels), len(values)))
		}
		samples = append(samples, sample{v, append([]string(nil), values...)})
	})
	sort.SliceStable(samples, func(i, j int) bool { return keyOf(samples[i].values) < keyOf(samples[j].values) })
	for _, s := range samples {
		writeSample(w, c.d.name, c.d.labels, s.values, "", "", s.v)
	}
}

// ---- text format

func writeSample(w *bufio.Writer, name string, labels, values []string, extraName, extraValue string, v float64) {
	w.WriteString(name)
	if len(labels) > 0 || extraName != "" {
		w.WriteByte('{')
		for i, l := range labels {
			if i > 0 {
				w.WriteByte(',')
			}
			w.WriteString(l)
			w.WriteString(`="`)
			w.WriteString(escapeLabel(values[i]))
			w.WriteByte('"')
		}
		if extraName != "" {
			if len(labels) > 0 {
				w.WriteByte(',')
			}
			w.WriteString(extraName)
			w.WriteString(`="`)
			w.WriteString(escapeLabel(extraValue))
			w.WriteByte('"')
		}
		w.WriteByte('}')
	}
	w.WriteByte(' ')
	w.WriteString(formatFloat(v))
	w.WriteByte('\n')
}

func formatFloat(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
var helpEscaper = strings.NewReplacer(`\`, `\\`, "\n", `\n`)

func escapeLabel(s string) string { return labelEscaper.Replace(strings.ToValidUTF8(s, "�")) }
func escapeHelp(s string) string  { return helpEscaper.Replace(strings.ToValidUTF8(s, "�")) }
