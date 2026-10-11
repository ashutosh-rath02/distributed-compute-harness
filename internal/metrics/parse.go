package metrics

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Sample is one parsed line of the exposition format.
type Sample struct {
	Name   string
	Labels map[string]string
	Value  float64
}

// Exposition is a parsed, validated scrape.
type Exposition struct {
	Types   map[string]string // family name -> counter|gauge|histogram
	Helps   map[string]string
	Samples []Sample
}

// Parse reads the text exposition format strictly: it is what the tests use to
// check that /metrics is something a real scraper accepts, so it rejects what
// Prometheus would (a sample before its TYPE, a duplicate series, a bad
// escape, histogram buckets that go down or lack +Inf, a missing final
// newline), rather than guessing.
func Parse(r io.Reader) (*Exposition, error) {
	br := bufio.NewReader(r)
	e := &Exposition{Types: map[string]string{}, Helps: map[string]string{}}
	seen := map[string]bool{}
	lineNo := 0
	for {
		line, err := br.ReadString('\n')
		if err == io.EOF {
			if line != "" {
				return nil, fmt.Errorf("line %d: missing final newline", lineNo+1)
			}
			break
		}
		if err != nil {
			return nil, err
		}
		lineNo++
		line = strings.TrimSuffix(line, "\n")
		fail := func(format string, a ...any) (*Exposition, error) {
			return nil, fmt.Errorf("line %d: %s: %q", lineNo, fmt.Sprintf(format, a...), line)
		}
		switch {
		case line == "":
			return fail("blank line")
		case strings.HasPrefix(line, "# HELP "):
			name, text, _ := strings.Cut(strings.TrimPrefix(line, "# HELP "), " ")
			if !nameRE.MatchString(name) {
				return fail("bad metric name in HELP")
			}
			if _, dup := e.Helps[name]; dup {
				return fail("duplicate HELP for %s", name)
			}
			e.Helps[name] = text
		case strings.HasPrefix(line, "# TYPE "):
			name, typ, ok := strings.Cut(strings.TrimPrefix(line, "# TYPE "), " ")
			if !ok || !nameRE.MatchString(name) {
				return fail("bad TYPE line")
			}
			switch typ {
			case "counter", "gauge", "histogram":
			default:
				return fail("unknown type %q", typ)
			}
			if _, dup := e.Types[name]; dup {
				return fail("duplicate TYPE for %s", name)
			}
			e.Types[name] = typ
		case strings.HasPrefix(line, "#"):
			// other comments are legal
		default:
			s, err := parseSample(line)
			if err != nil {
				return fail("%v", err)
			}
			if fam := familyOf(e.Types, s.Name); fam == "" {
				return fail("sample %s has no TYPE before it", s.Name)
			}
			key := s.Name + "{" + labelKey(s.Labels) + "}"
			if seen[key] {
				return fail("duplicate series")
			}
			seen[key] = true
			e.Samples = append(e.Samples, s)
		}
	}
	return e, e.checkHistograms()
}

// familyOf maps a sample name to its declared family, or "".
func familyOf(types map[string]string, name string) string {
	if _, ok := types[name]; ok {
		return name
	}
	for _, suffix := range []string{"_bucket", "_sum", "_count"} {
		if base, ok := strings.CutSuffix(name, suffix); ok && types[base] == "histogram" {
			return base
		}
	}
	return ""
}

func labelKey(l map[string]string) string {
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%q,", k, l[k])
	}
	return b.String()
}

func parseSample(line string) (Sample, error) {
	s := Sample{Labels: map[string]string{}}
	i := 0
	for i < len(line) && line[i] != '{' && line[i] != ' ' {
		i++
	}
	s.Name = line[:i]
	if !nameRE.MatchString(s.Name) {
		return s, fmt.Errorf("bad metric name")
	}
	if i < len(line) && line[i] == '{' {
		i++
		for {
			if i >= len(line) {
				return s, fmt.Errorf("unterminated label set")
			}
			if line[i] == '}' {
				i++
				break
			}
			j := i
			for j < len(line) && line[j] != '=' {
				j++
			}
			if j+1 >= len(line) || line[j+1] != '"' {
				return s, fmt.Errorf("label without a quoted value")
			}
			ln := line[i:j]
			if !labelRE.MatchString(ln) {
				return s, fmt.Errorf("bad label name %q", ln)
			}
			if _, dup := s.Labels[ln]; dup {
				return s, fmt.Errorf("duplicate label %s", ln)
			}
			j += 2
			var val strings.Builder
			for {
				if j >= len(line) {
					return s, fmt.Errorf("unterminated label value")
				}
				c := line[j]
				if c == '"' {
					j++
					break
				}
				if c == '\\' {
					if j+1 >= len(line) {
						return s, fmt.Errorf("dangling escape")
					}
					switch line[j+1] {
					case '\\':
						val.WriteByte('\\')
					case '"':
						val.WriteByte('"')
					case 'n':
						val.WriteByte('\n')
					default:
						return s, fmt.Errorf("bad escape \\%c", line[j+1])
					}
					j += 2
					continue
				}
				val.WriteByte(c)
				j++
			}
			s.Labels[ln] = val.String()
			i = j
			if i < len(line) && line[i] == ',' {
				i++
			}
		}
	}
	if i >= len(line) || line[i] != ' ' {
		return s, fmt.Errorf("no value")
	}
	rest := strings.TrimSpace(line[i:])
	if strings.ContainsAny(rest, " \t") {
		return s, fmt.Errorf("extra fields after the value (timestamps are not used here)")
	}
	switch rest {
	case "+Inf":
		s.Value = math.Inf(1)
	case "-Inf":
		s.Value = math.Inf(-1)
	case "NaN":
		s.Value = math.NaN()
	default:
		v, err := strconv.ParseFloat(rest, 64)
		if err != nil {
			return s, fmt.Errorf("bad value %q", rest)
		}
		s.Value = v
	}
	return s, nil
}

// checkHistograms verifies every histogram series: cumulative buckets that
// never decrease, a +Inf bucket, and +Inf equal to _count.
func (e *Exposition) checkHistograms() error {
	type bucket struct {
		le float64
		v  float64
	}
	buckets := map[string][]bucket{}
	counts := map[string]float64{}
	for _, s := range e.Samples {
		fam := familyOf(e.Types, s.Name)
		if e.Types[fam] != "histogram" {
			continue
		}
		rest := map[string]string{}
		for k, v := range s.Labels {
			if k != "le" {
				rest[k] = v
			}
		}
		id := fam + "{" + labelKey(rest) + "}"
		switch s.Name {
		case fam + "_bucket":
			le, ok := s.Labels["le"]
			if !ok {
				return fmt.Errorf("%s: bucket without le", id)
			}
			f, err := strconv.ParseFloat(le, 64)
			if err != nil {
				return fmt.Errorf("%s: bad le %q", id, le)
			}
			buckets[id] = append(buckets[id], bucket{f, s.Value})
		case fam + "_count":
			counts[id] = s.Value
		}
	}
	for id, bs := range buckets {
		sort.Slice(bs, func(i, j int) bool { return bs[i].le < bs[j].le })
		if !math.IsInf(bs[len(bs)-1].le, 1) {
			return fmt.Errorf("%s: no +Inf bucket", id)
		}
		for i := 1; i < len(bs); i++ {
			if bs[i].v < bs[i-1].v {
				return fmt.Errorf("%s: bucket le=%v has %v, fewer than the smaller bucket's %v", id, bs[i].le, bs[i].v, bs[i-1].v)
			}
		}
		if c, ok := counts[id]; !ok || c != bs[len(bs)-1].v {
			return fmt.Errorf("%s: +Inf bucket %v does not match _count %v", id, bs[len(bs)-1].v, counts[id])
		}
	}
	return nil
}

// Get returns the value of the series with exactly these labels
// (name, then key, value, key, value...).
func (e *Exposition) Get(name string, kv ...string) (float64, bool) {
	want := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		want[kv[i]] = kv[i+1]
	}
	for _, s := range e.Samples {
		if s.Name == name && labelKey(s.Labels) == labelKey(want) {
			return s.Value, true
		}
	}
	return 0, false
}

// Sum adds up every series of name whose labels include all the given ones.
func (e *Exposition) Sum(name string, kv ...string) float64 {
	var total float64
	for _, s := range e.Samples {
		if s.Name != name {
			continue
		}
		match := true
		for i := 0; i+1 < len(kv); i += 2 {
			if s.Labels[kv[i]] != kv[i+1] {
				match = false
			}
		}
		if match {
			total += s.Value
		}
	}
	return total
}

// Series counts the samples of name.
func (e *Exposition) Series(name string) int {
	n := 0
	for _, s := range e.Samples {
		if s.Name == name {
			n++
		}
	}
	return n
}
