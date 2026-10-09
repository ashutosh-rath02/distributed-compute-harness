package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildMapReduce(t *testing.T) {
	body, err := buildMapReduce("wc", 8, "lines", "text.count", "json.merge mode=sum field=total", 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(body)
	got := string(raw)
	for _, want := range []string{
		`"type":"text.split"`, `"chunks":"8"`, `"by":"lines"`,
		`"type":"text.count"`, `"mode":"perFile"`,
		`"combine":{"params":{"field":"total","mode":"sum"},"type":"json.merge"}`,
		`"maxAttempts":2`, `"name":"wc"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("workflow %s lacks %s", got, want)
		}
	}
	for name, c := range map[string]struct {
		chunks int
		by     string
		m, r   string
	}{
		"too many chunks": {17, "lines", "text.count", "json.merge"},
		"bad cut":         {4, "words", "text.count", "json.merge"},
		"empty map":       {4, "lines", "", "json.merge"},
		"bad reduce":      {4, "lines", "text.count", "json.merge oops"},
	} {
		if _, err := buildMapReduce("", c.chunks, c.by, c.m, c.r, 0, nil); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
