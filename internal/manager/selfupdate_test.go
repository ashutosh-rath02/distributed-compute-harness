package manager

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"home-harness/internal/domain"
)

func TestHashFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.exe")
	content := []byte("agent binary content")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := hashFile(path)
	if err != nil {
		t.Fatalf("hashFile: %v", err)
	}
	sum := sha256.Sum256(content)
	if want := hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("hashFile = %q, want %q", got, want)
	}
}

func testServerWithBuilds(builds ...agentBuild) *Server {
	return &Server{agents: &agentCatalog{builds: builds}, Registry: NewRegistry(), pending: make(map[string]pendingCommand)}
}

var (
	windowsBuild = agentBuild{OS: "windows", Arch: "amd64", SHA256: "windows-hash"}
	androidBuild = agentBuild{OS: "linux", Arch: "arm64", SHA256: "android-hash"}
)

func nodeOn(goos, arch, hash string, features ...string) *NodeRecord {
	return &NodeRecord{
		Node:          domain.Node{Platform: domain.Platform{OS: goos, Architecture: arch}, BinaryHash: hash},
		AgentFeatures: features,
	}
}

func TestUpdateStatus(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    *Server
		rec  *NodeRecord
		want UpdateStatus
	}{
		{"no builds loaded", testServerWithBuilds(), nodeOn("windows", "amd64", "old", domain.FeatureSelfUpdatePath), UpdateUnknown},
		{"node never reported a hash", testServerWithBuilds(windowsBuild), nodeOn("windows", "amd64", ""), UpdateUnknown},
		{"already current", testServerWithBuilds(windowsBuild), nodeOn("windows", "amd64", "windows-hash", domain.FeatureSelfUpdatePath), UpdateCurrent},
		{"different build, current agent", testServerWithBuilds(windowsBuild, androidBuild), nodeOn("linux", "arm64", "old", domain.FeatureSelfUpdatePath), UpdateAvailable},
		// A pre-catalog agent fetches the legacy route, which serves the
		// primary (windows) build: fine for a Windows node...
		{"legacy agent on primary platform", testServerWithBuilds(androidBuild, windowsBuild), nodeOn("windows", "amd64", "old"), UpdateAvailable},
		// ...but an Android one would only ever download Windows bytes.
		{"legacy agent off primary platform", testServerWithBuilds(windowsBuild, androidBuild), nodeOn("linux", "arm64", "old"), UpdateReinstallRequired},
		// The bricking regression: a Windows-only catalog must never offer
		// its build to an Android node, however different the hashes are.
		{"no build for node platform", testServerWithBuilds(windowsBuild), nodeOn("linux", "arm64", "old", domain.FeatureSelfUpdatePath), UpdateUnknown},
	} {
		if got := tc.s.UpdateStatusFor(tc.rec); got != tc.want {
			t.Errorf("%s: UpdateStatus = %s, want %s", tc.name, got, tc.want)
		}
	}
}

// recordingConn is a domain.Conn that records everything sent on it.
type recordingConn struct{ sent chan []byte }

func (c *recordingConn) Send(ctx context.Context, data []byte) error {
	c.sent <- data
	return nil
}
func (c *recordingConn) Receive(ctx context.Context) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (c *recordingConn) RemoteAddr() string { return "test" }
func (c *recordingConn) Close() error       { return nil }

// TestUpdateAPIRefusesCrossPlatformBuild is the end-to-end form of the
// bricking regression: POST /nodes/{id}/update for an Android node on a
// manager serving only a Windows build must refuse without ever sending
// SELF_UPDATE.
func TestUpdateAPIRefusesCrossPlatformBuild(t *testing.T) {
	s := testServerWithBuilds(windowsBuild)
	conn := &recordingConn{sent: make(chan []byte, 4)}
	manifest := domain.Manifest{
		Node:          domain.Node{Identity: domain.Identity{NodeID: "node-phone"}, Platform: domain.Platform{OS: "linux", Architecture: "arm64"}, BinaryHash: "old-android-hash"},
		AgentFeatures: []string{domain.FeatureSelfUpdatePath},
	}
	s.Registry.Upsert(manifest, conn)
	s.Registry.SetState("node-phone", domain.NodeReady)

	// Routed through a mux so r.PathValue("id") is populated.
	mux := http.NewServeMux()
	mux.HandleFunc("POST /nodes/{id}/update", s.apiPostUpdate)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/nodes/node-phone/update", nil))

	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "linux/arm64") {
		t.Fatalf("expected 409 naming the missing linux/arm64 build, got %d %q", rec.Code, rec.Body.String())
	}
	select {
	case msg := <-conn.sent:
		t.Fatalf("expected no SELF_UPDATE to be dispatched to the Android node, but sent: %s", msg)
	default:
	}
}
