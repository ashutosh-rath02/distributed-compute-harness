package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRunHealthcheck(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/server-proof" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Write([]byte(`{"proof":"x"}`))
	}))
	defer up.Close()
	if code := runHealthcheck(strings.TrimPrefix(up.URL, "http://")); code != 0 {
		t.Fatalf("healthy manager reported code %d", code)
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "x", 500) }))
	defer bad.Close()
	if code := runHealthcheck(strings.TrimPrefix(bad.URL, "http://")); code != 1 {
		t.Fatal("a 500 must be unhealthy")
	}
	if code := runHealthcheck("127.0.0.1:19769"); code != 1 {
		t.Fatal("nothing listening must be unhealthy")
	}
}
