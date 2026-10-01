package manager

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"home-harness/internal/joinscript"
)

func TestEnrollmentTokenIsSingleUseAndExpires(t *testing.T) {
	store := newEnrollmentStore()
	e, err := store.create("https://phone:7420", "android", joinscript.ModeLAN, time.Minute)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, ok := store.get(e.Token); !ok {
		t.Fatal("fresh token should be valid")
	}
	if !store.consume(e.Token) || store.consume(e.Token) {
		t.Fatal("token should succeed exactly once")
	}
	expired, err := store.create("https://phone:7420", "windows", joinscript.ModeLAN, time.Nanosecond)
	if err != nil {
		t.Fatalf("create expired: %v", err)
	}
	time.Sleep(time.Millisecond)
	if _, ok := store.get(expired.Token); ok {
		t.Fatal("expired token should be rejected")
	}
}

func TestEnrollmentAPIAndPublicBootstrapDoNotExposePermanentToken(t *testing.T) {
	dir := t.TempDir()
	binaryPath := filepath.Join(dir, "agent.exe")
	if err := os.WriteFile(binaryPath, []byte("agent content"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewServer(nil, nil, Config{
		PairingToken: "permanent-secret", Fingerprint: "fingerprint",
		AgentBinaryPath: binaryPath, EnrollmentTTL: time.Minute,
	})
	api := httptest.NewServer(s.NewHTTPHandler())
	defer api.Close()
	body, _ := json.Marshal(createEnrollmentRequest{Addr: "192.168.1.10:7420", Platform: "windows"})
	resp, err := http.Post(api.URL+"/enrollments", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST enrollment: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		got, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, got)
	}
	var view enrollmentView
	if err := json.NewDecoder(resp.Body).Decode(&view); err != nil {
		t.Fatal(err)
	}
	if view.Token == "" || !strings.Contains(view.URL, view.Token) || strings.Contains(view.URL, "permanent-secret") {
		t.Fatalf("unsafe or incomplete enrollment view: %+v", view)
	}
	qrResp, err := http.Get(api.URL + view.QRPath)
	if err != nil {
		t.Fatalf("GET QR: %v", err)
	}
	qrBody, _ := io.ReadAll(qrResp.Body)
	qrResp.Body.Close()
	if qrResp.StatusCode != http.StatusOK || !strings.Contains(qrResp.Header.Get("Content-Type"), "image/svg+xml") || !bytes.Contains(qrBody, []byte("<svg")) {
		t.Fatalf("invalid QR response: status=%d content-type=%q", qrResp.StatusCode, qrResp.Header.Get("Content-Type"))
	}

	public := httptest.NewServer(s.EnrollmentHandler())
	defer public.Close()
	setupResp, err := http.Get(public.URL + "/enroll/" + view.Token + "/setup")
	if err != nil {
		t.Fatal(err)
	}
	defer setupResp.Body.Close()
	script, _ := io.ReadAll(setupResp.Body)
	if setupResp.StatusCode != http.StatusOK || !strings.Contains(string(script), view.Token) {
		t.Fatalf("expected token-scoped bootstrap, status=%d body=%s", setupResp.StatusCode, script)
	}
	if strings.Contains(string(script), "permanent-secret") {
		t.Fatal("bootstrap exposed permanent pairing token")
	}
}

func TestCreateEnrollmentRejectsShellLikeAddress(t *testing.T) {
	s := NewServer(nil, nil, Config{})
	s.agentBinaryHash = "hash"
	api := httptest.NewServer(s.NewHTTPHandler())
	defer api.Close()
	body := []byte(`{"addr":"phone;whoami:7420","platform":"windows"}`)
	resp, err := http.Post(api.URL+"/enrollments", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestCreateEnrollmentRejectsWrongBinaryPlatform(t *testing.T) {
	s := NewServer(nil, nil, Config{})
	s.agentBinaryHash, s.agentBinaryOS, s.agentBinaryArch = "hash", "windows", "amd64"
	api := httptest.NewServer(s.NewHTTPHandler())
	defer api.Close()
	body := []byte(`{"addr":"192.168.1.10:7420","platform":"android"}`)
	resp, err := http.Post(api.URL+"/enrollments", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409, got %d", resp.StatusCode)
	}
}
