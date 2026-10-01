package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestParseParams(t *testing.T) {
	params, err := parseParams([]string{"path=/tmp/file.txt", "encoding=utf-8"})
	if err != nil {
		t.Fatalf("parseParams: %v", err)
	}
	want := map[string]string{"path": "/tmp/file.txt", "encoding": "utf-8"}
	if !reflect.DeepEqual(params, want) {
		t.Fatalf("parseParams = %+v, want %+v", params, want)
	}
}

func TestParseParamsAllowsEmptyValue(t *testing.T) {
	params, err := parseParams([]string{"flag="})
	if err != nil {
		t.Fatalf("parseParams: %v", err)
	}
	if params["flag"] != "" {
		t.Fatalf("expected empty value for %q, got %+v", "flag=", params)
	}
}

func TestParseParamsNoArgsReturnsNil(t *testing.T) {
	params, err := parseParams(nil)
	if err != nil {
		t.Fatalf("parseParams: %v", err)
	}
	if params != nil {
		t.Fatalf("expected nil params for no args, got %+v", params)
	}
}

func TestParseParamsRejectsMissingEquals(t *testing.T) {
	if _, err := parseParams([]string{"not-a-key-value-pair"}); err == nil {
		t.Fatal("expected an error for a param without '='")
	}
}

func TestTruncate(t *testing.T) {
	cases := map[string]string{
		"short":                "short",
		"exactly-20-chars0000": "exactly-20-chars0000",
	}
	for in, want := range cases {
		if got := truncate(in, 30); got != want {
			t.Fatalf("truncate(%q, 30) = %q, want %q", in, got, want)
		}
	}
	if got := truncate("this is definitely longer than ten", 10); len([]rune(got)) != 10 {
		t.Fatalf("expected truncated result of length 10, got %q (len %d)", got, len([]rune(got)))
	}
}

func TestParseBytes(t *testing.T) {
	cases := []struct {
		in   string
		want uint64
	}{
		{"", 0},
		{"1024", 1024},
		{"2KiB", 2 << 10},
		{"2kib", 2 << 10},
		{"2KB", 2 << 10},
		{"512MiB", 512 << 20},
		{"1GiB", 1 << 30},
		{"1.5G", uint64(1.5 * (1 << 30))},
		{"1TB", 1 << 40},
	}
	for _, c := range cases {
		got, err := parseBytes(c.in)
		if err != nil {
			t.Fatalf("parseBytes(%q): %v", c.in, err)
		}
		if got != c.want {
			t.Fatalf("parseBytes(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestParseBytesRejectsGarbage(t *testing.T) {
	for _, in := range []string{"abc", "12XB", "-5GiB"} {
		if _, err := parseBytes(in); err == nil {
			t.Fatalf("expected parseBytes(%q) to fail", in)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   uint64
		want string
	}{
		{500, "500 B"},
		{2048, "2.0 KiB"},
		{1 << 30, "1.0 GiB"},
	}
	for _, c := range cases {
		if got := humanBytes(c.in); got != c.want {
			t.Fatalf("humanBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParseTypedAllowsFlagsAfterParameters(t *testing.T) {
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	in := fs.String("in", "", "")
	rt := fs.String("reduce-type", "", "")
	pos, err := parseTyped(fs, []string{"image.resize", "width=800", "-in", "a.jpg", "format=png", "-reduce-type", "archive.zip"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(pos, " ") != "image.resize width=800 format=png" || *in != "a.jpg" || *rt != "archive.zip" {
		t.Fatalf("pos %v in %q reduce-type %q", pos, *in, *rt)
	}
}

// map's own -type switches to typed parameters; a raw command's own
// "-type f" (find) must stay part of that command.
func TestMapKeepsARawCommandsFlagsAndParsesTypedJobs(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		bodies = append(bodies, b)
		w.WriteHeader(http.StatusAccepted)
		io.WriteString(w, `{"id":"j"}`)
	}))
	defer srv.Close()
	c := newAPIClient(srv.URL, "")
	if err := cmdMap(c, []string{"-count", "1", "find", ".", "-type", "f"}); err != nil {
		t.Fatal(err)
	}
	if err := cmdMap(c, []string{"-type", "cpu.burn", "-count", "2", "seconds=3", "-attempts", "2"}); err != nil {
		t.Fatal(err)
	}
	raw := bodies[0]["tasks"].([]any)[0].(map[string]any)
	if raw["command"] != "find" || fmt.Sprint(raw["args"]) != "[. -type f]" || raw["capability"] != nil {
		t.Fatalf("raw task %v", raw)
	}
	typed := bodies[1]["tasks"].([]any)
	first := typed[0].(map[string]any)
	if len(typed) != 2 || first["capability"] != "cpu.burn" || first["params"].(map[string]any)["seconds"] != "3" || bodies[1]["maxAttempts"] != float64(2) {
		t.Fatalf("typed job %v", bodies[1])
	}
}
