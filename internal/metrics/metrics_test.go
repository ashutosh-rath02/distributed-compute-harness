package metrics

import (
	"bytes"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
)

func render(t *testing.T, r *Registry) string {
	t.Helper()
	var buf bytes.Buffer
	if _, err := r.WriteTo(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestExpositionFormatExact(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounterVec("demo_requests_total", "Requests.", "route", "code")
	c.With("/a", "2xx").Add(3)
	c.With("/a", "5xx").Inc()
	g := r.NewGaugeVec("demo_queue", "Queue depth.")
	g.With().Set(7)
	got := render(t, r)
	want := `# HELP demo_queue Queue depth.
# TYPE demo_queue gauge
demo_queue 7
# HELP demo_requests_total Requests.
# TYPE demo_requests_total counter
demo_requests_total{route="/a",code="2xx"} 3
demo_requests_total{route="/a",code="5xx"} 1
`
	if got != want {
		t.Fatalf("exposition:\n%s\nwant:\n%s", got, want)
	}
	if _, err := Parse(strings.NewReader(got)); err != nil {
		t.Fatalf("own output does not parse: %v", err)
	}
}

func TestLabelValueEscaping(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounterVec("demo_total", "A \\ help\nwith newline.", "v")
	nasty := "back\\slash \"quote\" new\nline } { , = \xff"
	c.With(nasty).Inc()
	out := render(t, r)
	if strings.Count(out, "\n") != 3 {
		t.Fatalf("a newline in a value or help leaked into the output:\n%q", out)
	}
	e, err := Parse(strings.NewReader(out))
	if err != nil {
		t.Fatalf("parse: %v\n%s", err, out)
	}
	if len(e.Samples) != 1 {
		t.Fatalf("samples: %+v", e.Samples)
	}
	// The invalid UTF-8 byte is replaced; everything else round-trips.
	want := strings.ToValidUTF8(nasty, "\uFFFD")
	if got := e.Samples[0].Labels["v"]; got != want {
		t.Fatalf("label round trip: got %q want %q", got, want)
	}
}

func TestHistogramBuckets(t *testing.T) {
	r := NewRegistry()
	h := r.NewHistogramVec("demo_seconds", "Latency.", []float64{0.1, 1, 10}, "kind")
	for _, v := range []float64{0.05, 0.1, 0.5, 5, 50, 50} {
		h.With("x").Observe(v)
	}
	h.With("x").Observe(math.NaN()) // ignored
	e, err := Parse(strings.NewReader(render(t, r)))
	if err != nil {
		t.Fatal(err)
	}
	for le, want := range map[string]float64{"0.1": 2, "1": 3, "10": 4, "+Inf": 6} {
		if got, _ := e.Get("demo_seconds_bucket", "kind", "x", "le", le); got != want {
			t.Errorf("bucket le=%s: got %v want %v", le, got, want)
		}
	}
	if got, _ := e.Get("demo_seconds_count", "kind", "x"); got != 6 {
		t.Errorf("count: %v", got)
	}
	if got, _ := e.Get("demo_seconds_sum", "kind", "x"); math.Abs(got-105.65) > 1e-9 {
		t.Errorf("sum: %v", got)
	}
}

func TestSeriesCapFoldsIntoOther(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounterVec("demo_total", "x", "id").SetMaxSeries(5)
	for i := 0; i < 1000; i++ {
		c.With(fmt.Sprintf("id-%d", i)).Inc()
	}
	e, err := Parse(strings.NewReader(render(t, r)))
	if err != nil {
		t.Fatal(err)
	}
	if n := e.Series("demo_total"); n != 6 {
		t.Fatalf("want 5 series plus one overflow, got %d", n)
	}
	if got := e.Sum("demo_total"); got != 1000 {
		t.Fatalf("nothing may be lost to the cap: total %v", got)
	}
	if got, _ := e.Get("demo_total", "id", OverflowLabel); got != 995 {
		t.Fatalf("overflow series: %v", got)
	}
}

func TestCounterNeverGoesDown(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounterVec("demo_total", "x").With()
	c.Add(5)
	c.Add(-3)
	if c.Value() != 5 {
		t.Fatalf("a counter must ignore negative adds, got %v", c.Value())
	}
}

func TestCollectorIsComputedAtScrape(t *testing.T) {
	r := NewRegistry()
	n := 1.0
	r.NewCollector("demo_nodes", "Nodes.", "gauge", []string{"state"}, func(emit func(float64, ...string)) {
		emit(n, "ready")
		emit(2*n, "offline")
	})
	a := render(t, r)
	n = 4
	b := render(t, r)
	if a == b || !strings.Contains(b, `demo_nodes{state="offline"} 8`) {
		t.Fatalf("collector was not re-run:\n%s\n%s", a, b)
	}
}

func TestConcurrentUse(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounterVec("demo_total", "x", "k")
	h := r.NewHistogramVec("demo_seconds", "x", []float64{1, 2}, "k")
	g := r.NewGaugeVec("demo_g", "x")
	var wg sync.WaitGroup
	for w := 0; w < 16; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				k := fmt.Sprintf("k%d", i%7)
				c.With(k).Inc()
				h.With(k).Observe(float64(i % 3))
				g.With().Add(1)
				if i%500 == 0 {
					var buf bytes.Buffer
					r.WriteTo(&buf) // scraping while writing
				}
			}
		}(w)
	}
	wg.Wait()
	e, err := Parse(strings.NewReader(render(t, r)))
	if err != nil {
		t.Fatal(err)
	}
	if got := e.Sum("demo_total"); got != 16*2000 {
		t.Fatalf("lost increments: %v", got)
	}
	if got := e.Sum("demo_seconds_count"); got != 16*2000 {
		t.Fatalf("lost observations: %v", got)
	}
	if v, _ := e.Get("demo_g"); v != 16*2000 {
		t.Fatalf("gauge: %v", v)
	}
}

func TestInvalidNamesPanic(t *testing.T) {
	for name, f := range map[string]func(*Registry){
		"metric name":    func(r *Registry) { r.NewCounterVec("bad-name", "x") },
		"label name":     func(r *Registry) { r.NewCounterVec("ok", "x", "bad-label") },
		"reserved le":    func(r *Registry) { r.NewHistogramVec("ok", "x", nil, "le") },
		"duplicate":      func(r *Registry) { r.NewCounterVec("ok", "x"); r.NewCounterVec("ok", "x") },
		"unsorted bucks": func(r *Registry) { r.NewHistogramVec("ok", "x", []float64{2, 1}) },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("want a panic")
				}
			}()
			f(NewRegistry())
		})
	}
}

func TestParserIsStrict(t *testing.T) {
	bad := map[string]string{
		"no type":           "x 1\n",
		"no final newline":  "# TYPE x counter\nx 1",
		"duplicate series":  "# TYPE x counter\nx 1\nx 2\n",
		"bad escape":        "# TYPE x counter\nx{a=\"\\q\"} 1\n",
		"unterminated":      "# TYPE x counter\nx{a=\"v} 1\n",
		"bad value":         "# TYPE x counter\nx one\n",
		"timestamp":         "# TYPE x counter\nx 1 1700000000\n",
		"decreasing bucket": "# TYPE h histogram\nh_bucket{le=\"1\"} 5\nh_bucket{le=\"+Inf\"} 3\nh_sum 1\nh_count 3\n",
		"no +Inf":           "# TYPE h histogram\nh_bucket{le=\"1\"} 5\nh_sum 1\nh_count 5\n",
		"count mismatch":    "# TYPE h histogram\nh_bucket{le=\"+Inf\"} 5\nh_sum 1\nh_count 4\n",
		"blank line":        "# TYPE x counter\n\nx 1\n",
	}
	for name, in := range bad {
		if _, err := Parse(strings.NewReader(in)); err == nil {
			t.Errorf("%s: accepted %q", name, in)
		}
	}
}
