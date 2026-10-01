package agent

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"home-harness/internal/domain"
)

func TestHTTPTransferFetchVerifiesBeforePlacingTheFile(t *testing.T) {
	content := []byte("the real input")
	serve := content
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		w.Write(serve)
	}))
	defer srv.Close()
	xfer := &httpTransfer{client: srv.Client(), base: srv.URL, workload: fileWorkloadID, token: "tok"}
	ref := domain.ArtifactRef{Name: "in.txt", SHA256: shaOf(content), Size: int64(len(content))}
	dest := filepath.Join(t.TempDir(), "in.txt")

	if err := xfer.Fetch(context.Background(), ref, dest); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if b, _ := os.ReadFile(dest); string(b) != string(content) {
		t.Fatalf("placed %q", b)
	}
	if gotAuth != "Bearer tok" || gotPath != "/workload-artifacts/"+string(fileWorkloadID)+"/inputs/"+ref.SHA256 {
		t.Fatalf("request: auth %q path %q", gotAuth, gotPath)
	}

	for name, body := range map[string][]byte{
		"same size, different bytes": []byte("the fake input"),
		"truncated":                  content[:5],
		"longer":                     append(append([]byte{}, content...), 'x'),
	} {
		serve = body
		bad := filepath.Join(t.TempDir(), "bad.txt")
		if err := xfer.Fetch(context.Background(), ref, bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if _, err := os.Stat(bad); !os.IsNotExist(err) {
			t.Errorf("%s: an unverified file was left at its real name", name)
		}
		if _, err := os.Stat(bad + ".part"); !os.IsNotExist(err) {
			t.Errorf("%s: partial download left behind", name)
		}
	}
}

func TestHTTPTransferUploadSendsHashAndChecksReply(t *testing.T) {
	content := []byte("an output")
	reply := `{"sha256":"` + shaOf(content) + `","size":9}`
	var gotSHA, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || !strings.HasSuffix(r.URL.Path, "/outputs/out/result.txt") {
			http.Error(w, "wrong route", 404)
			return
		}
		gotSHA = r.Header.Get("X-Artifact-SHA256")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, reply)
	}))
	defer srv.Close()
	src := filepath.Join(t.TempDir(), "result.txt")
	os.WriteFile(src, content, 0o600)
	xfer := &httpTransfer{client: srv.Client(), base: srv.URL, workload: fileWorkloadID, token: "tok"}

	ref, err := xfer.Upload(context.Background(), "out/result.txt", src)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if gotSHA != shaOf(content) || gotBody != string(content) || ref.SHA256 != shaOf(content) || ref.Size != 9 {
		t.Fatalf("sent sha %q body %q, got ref %+v", gotSHA, gotBody, ref)
	}
	reply = `{"sha256":"` + strings.Repeat("0", 64) + `","size":9}`
	if _, err := xfer.Upload(context.Background(), "out/result.txt", src); err == nil {
		t.Fatal("an upload the manager stored differently was reported as delivered")
	}
}

// The manager reserves each upload's declared Content-Length against the
// attempt's budget, so even an empty output must declare one.
func TestHTTPTransferUploadDeclaresLengthForEmptyFile(t *testing.T) {
	var gotLength int64 = -2
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotLength = r.ContentLength
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"sha256":"`+shaOf(nil)+`","size":0}`)
	}))
	defer srv.Close()
	src := filepath.Join(t.TempDir(), "empty.txt")
	os.WriteFile(src, nil, 0o600)
	xfer := &httpTransfer{client: srv.Client(), base: srv.URL, workload: fileWorkloadID, token: "tok"}
	if _, err := xfer.Upload(context.Background(), "empty.txt", src); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if gotLength != 0 {
		t.Fatalf("server saw Content-Length %d, want 0", gotLength)
	}
}
