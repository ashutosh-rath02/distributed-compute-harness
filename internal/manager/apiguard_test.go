package manager

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOperatorHostAllowed(t *testing.T) {
	for _, tc := range []struct {
		host string
		want bool
	}{
		{"127.0.0.1:7421", true},
		{"[::1]:7421", true},
		{"localhost:7421", true},
		{"LocalHost:7421", true},
		{"localhost.:7421", true},
		{"192.168.10.11:7421", true}, // an IP literal can't be rebound
		{"", true},
		{"attacker.example:7421", false},
		{"attacker.example", false},
		{"localhost.attacker.example:7421", false},
		{"127.0.0.1.nip.io:7421", false},
	} {
		if got := operatorHostAllowed(tc.host); got != tc.want {
			t.Errorf("operatorHostAllowed(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}

// TestOperatorAPIRejectsBrowserBorneAttacks drives the real operator
// handler: a cross-site page must not be able to submit a workload (which
// would run arbitrary code on the fleet) or, via DNS rebinding, read the
// pairing token — while harnessctl-style and same-origin dashboard
// requests keep working.
func TestOperatorAPIRejectsBrowserBorneAttacks(t *testing.T) {
	s := NewServer(nil, nil, Config{PairingToken: "permanent-secret"})
	h := s.NewHTTPHandler()

	do := func(method, target, host string, headers map[string]string, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, strings.NewReader(body))
		req.Host = host
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	workload := `{"command":"calc.exe"}`

	// The no-preflight CSRF shape: text/plain body, marked cross-site.
	rec := do(http.MethodPost, "/workloads", "127.0.0.1:7421",
		map[string]string{"Content-Type": "text/plain", "Sec-Fetch-Site": "cross-site", "Origin": "https://attacker.example"}, workload)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site POST /workloads: got %d, want 403", rec.Code)
	}
	// Older browser without Sec-Fetch-Site: Origin alone must be enough.
	rec = do(http.MethodPost, "/workloads", "127.0.0.1:7421",
		map[string]string{"Content-Type": "text/plain", "Origin": "https://attacker.example"}, workload)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin POST /workloads (Origin only): got %d, want 403", rec.Code)
	}
	if n := len(s.Workloads.List()); n != 0 {
		t.Fatalf("expected no workload to be created by rejected requests, got %d", n)
	}

	// DNS rebinding: same-origin from the browser's view, but Host is the
	// attacker's name — even a safe GET must not leak the pairing token.
	rec = do(http.MethodGet, "/join-info", "attacker.example:7421",
		map[string]string{"Sec-Fetch-Site": "same-origin"}, "")
	if rec.Code != http.StatusForbidden || strings.Contains(rec.Body.String(), "permanent-secret") {
		t.Fatalf("rebound GET /join-info: got %d %q, want 403 without the token", rec.Code, rec.Body.String())
	}

	// Legitimate callers are unaffected. harnessctl sends neither header;
	// the dashboard's own fetches are same-origin. Both reach the handler
	// (SubmitWorkload then fails with no nodes, which is the handler's
	// own answer — not the guard's 403).
	rec = do(http.MethodPost, "/workloads", "127.0.0.1:7421",
		map[string]string{"Content-Type": "application/json"}, workload)
	if rec.Code == http.StatusForbidden {
		t.Fatalf("CLI-style POST /workloads was blocked by the guard: %d %s", rec.Code, rec.Body.String())
	}
	rec = do(http.MethodPost, "/workloads", "localhost:7421",
		map[string]string{"Content-Type": "application/json", "Sec-Fetch-Site": "same-origin", "Origin": "http://localhost:7421"}, workload)
	if rec.Code == http.StatusForbidden {
		t.Fatalf("same-origin dashboard POST /workloads was blocked by the guard: %d %s", rec.Code, rec.Body.String())
	}
	rec = do(http.MethodGet, "/join-info", "127.0.0.1:7421", nil, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "permanent-secret") {
		t.Fatalf("loopback GET /join-info: got %d, want 200 with the token", rec.Code)
	}
}
