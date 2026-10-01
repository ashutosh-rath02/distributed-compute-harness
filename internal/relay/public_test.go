package relay

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPublicGatewayRejectsWrongPublishCredential(t *testing.T) {
	gateway := NewPublicGateway(NewServerWithAliasKey(0, "alias-key"), "127.0.0.1:1", "correct-secret")
	req := httptest.NewRequest(http.MethodPost, "/_harness/enrollments", bytes.NewBufferString(`{"token":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`))
	req.Header.Set("Authorization", "Bearer wrong-secret")
	rec := httptest.NewRecorder()
	gateway.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401", rec.Code)
	}
}
