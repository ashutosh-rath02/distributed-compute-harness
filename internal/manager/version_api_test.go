package manager

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// GET /version needs no credential, reports only the build identity, and
// opening it did not open anything next to it.
func TestVersionRouteIsPublicAndHarmless(t *testing.T) {
	s := NewServer(nil, nil, Config{OperatorToken: testOperatorToken, PairingToken: "permanent-pairing-secret"})
	h := s.NewHTTPHandler()

	rec := operatorRequest(t, h, http.MethodGet, "/version", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /version without a credential: got %d", rec.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	for _, key := range []string{"version", "platform"} {
		if got[key] == "" || got[key] == nil {
			t.Errorf("missing %q in %v", key, got)
		}
	}
	body := rec.Body.String()
	if strings.Contains(body, testOperatorToken) || strings.Contains(body, "permanent-pairing-secret") {
		t.Errorf("GET /version leaked a secret: %s", body)
	}

	// Only GET on exactly /version is public.
	for _, route := range []struct{ method, path string }{
		{http.MethodPost, "/version"},
		{http.MethodGet, "/version/x"},
		{http.MethodGet, "/versions"},
		{http.MethodGet, "/nodes"},
	} {
		if rec := operatorRequest(t, h, route.method, route.path, "", ""); rec.Code == http.StatusOK {
			t.Errorf("%s %s answered 200 without a credential", route.method, route.path)
		}
	}
}
