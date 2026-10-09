package logging

import (
	"bytes"
	"encoding/json"
	"log"
	"log/slog"
	"regexp"
	"strings"
	"testing"
)

func capture(t *testing.T, format, level string) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev); log.SetFlags(log.LstdFlags) })
	if err := Setup(format, level, &buf); err != nil {
		t.Fatal(err)
	}
	return &buf
}

func TestTextKeepsTheOldShape(t *testing.T) {
	buf := capture(t, "text", "info")
	log.Printf("node.registered: %s (%s)", "n1", "My PC") // the old way
	slog.Info("workload.completed", "workload", "w1", "node", "n 1", "reason", "")
	slog.Warn("careful", "capability", "image.resize")
	slog.Debug("hidden at info")
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines: %q", lines)
	}
	ts := regexp.MustCompile(`^\d{4}/\d\d/\d\d \d\d:\d\d:\d\d `)
	for _, l := range lines {
		if !ts.MatchString(l) {
			t.Errorf("no standard-logger timestamp: %q", l)
		}
	}
	if !strings.HasSuffix(lines[0], " node.registered: n1 (My PC)") {
		t.Errorf("a log.Printf line changed: %q", lines[0])
	}
	if !strings.HasSuffix(lines[1], ` workload.completed workload=w1 node="n 1" reason=""`) {
		t.Errorf("fields: %q", lines[1])
	}
	if !strings.Contains(lines[2], " [WARN] careful capability=image.resize") {
		t.Errorf("level tag: %q", lines[2])
	}
}

func TestJSONRecords(t *testing.T) {
	buf := capture(t, "json", "debug")
	slog.Info("job.finished", "job", "j1", "state", "COMPLETED")
	log.Printf("plain %d", 7)
	var rec map[string]any
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatal(err)
	}
	if rec["msg"] != "job.finished" || rec["job"] != "j1" || rec["level"] != "INFO" {
		t.Fatalf("record: %v", rec)
	}
	if err := json.Unmarshal([]byte(lines[1]), &rec); err != nil || rec["msg"] != "plain 7" {
		t.Fatalf("log.Printf as JSON: %v %v", rec, err)
	}
}

func TestSecretsAreRedactedInBothFormats(t *testing.T) {
	for _, format := range []string{"text", "json"} {
		buf := capture(t, format, "info")
		slog.Info("oops", "token", "sekrit-1", "prompt", "tell me sekrit-2", "argv", []string{"sekrit-3"},
			"Authorization", "Bearer sekrit-4", "command", "sekrit-5", "node", "n1")
		out := buf.String()
		if strings.Contains(out, "sekrit") {
			t.Errorf("%s: a secret got through: %s", format, out)
		}
		if !strings.Contains(out, "n1") {
			t.Errorf("%s: ordinary field lost: %s", format, out)
		}
	}
}

func TestLevelFilterAndBadFlags(t *testing.T) {
	buf := capture(t, "text", "warn")
	slog.Info("quiet")
	log.Printf("also quiet") // the standard logger is INFO
	slog.Error("loud")
	if out := buf.String(); strings.Contains(out, "quiet") || !strings.Contains(out, "loud") {
		t.Fatalf("level filter: %q", out)
	}
	if err := Setup("yaml", "info", &bytes.Buffer{}); err == nil {
		t.Error("bad format accepted")
	}
	if err := Setup("text", "loud", &bytes.Buffer{}); err == nil {
		t.Error("bad level accepted")
	}
}

func TestWithAttrsAndGroups(t *testing.T) {
	buf := capture(t, "text", "info")
	slog.Default().With("node", "n1").WithGroup("g").Info("hello", "a", 1)
	if !strings.HasSuffix(strings.TrimSpace(buf.String()), "hello node=n1 g.a=1") {
		t.Fatalf("%q", buf.String())
	}
}
