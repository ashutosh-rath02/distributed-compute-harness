package main

import (
	"reflect"
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
