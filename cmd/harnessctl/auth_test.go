package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBearerTransportSendsTokenAndExplains401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer right-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	if err := newAPIClient(srv.URL, "right-token").get("/nodes", new([]nodeView)); err != nil {
		t.Fatalf("expected the bearer token to be accepted: %v", err)
	}
	err := newAPIClient(srv.URL, "").get("/nodes", new([]nodeView))
	if err == nil || !strings.Contains(err.Error(), operatorTokenEnv) {
		t.Fatalf("expected a 401 without a token to explain how to provide one, got %v", err)
	}
	err = newAPIClient(srv.URL, "stale-token").get("/nodes", new([]nodeView))
	if err == nil || !strings.Contains(err.Error(), "rejected this operator token") {
		t.Fatalf("expected a 401 with a token to say it was rejected, got %v", err)
	}
}

func TestLoadOperatorTokenPrefersEnvOverFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "harness-operator-token")
	os.WriteFile(file, []byte("from-file\n"), 0o600)
	t.Setenv(operatorTokenEnv, "")
	if got := loadOperatorToken(file); got != "from-file" {
		t.Fatalf("file token: got %q", got)
	}
	t.Setenv(operatorTokenEnv, "from-env")
	if got := loadOperatorToken(file); got != "from-env" {
		t.Fatalf("env token: got %q", got)
	}
	t.Setenv(operatorTokenEnv, "")
	if got := loadOperatorToken(filepath.Join(t.TempDir(), "missing")); got != "" {
		t.Fatalf("missing file: got %q", got)
	}
}
