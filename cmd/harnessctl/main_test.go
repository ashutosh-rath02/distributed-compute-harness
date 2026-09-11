package main

import "testing"

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
