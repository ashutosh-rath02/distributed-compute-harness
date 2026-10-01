package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"home-harness/internal/domain"
)

// httpTransfer moves one assignment's files over HTTP on the same route
// to the manager this agent's connection uses (direct pinned TLS, or the
// relay), authenticated by the assignment's token (agent-facing routes in
// the manager's artifacts.go).
type httpTransfer struct {
	client   *http.Client
	base     string
	workload domain.WorkloadID
	token    string
}

func (t *httpTransfer) url(kind, last string) string {
	return t.base + "/workload-artifacts/" + string(t.workload) + "/" + kind + "/" + last
}

func (t *httpTransfer) do(req *http.Request) (*http.Response, error) {
	req.Header.Set("Authorization", "Bearer "+t.token)
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return nil, fmt.Errorf("manager answered %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return resp, nil
}

// Fetch downloads ref to dest, verifying its size and SHA-256 before the
// file appears under its real name.
func (t *httpTransfer) Fetch(ctx context.Context, ref domain.ArtifactRef, dest string) error {
	if !domain.ValidSHA256(ref.SHA256) {
		return errors.New("invalid sha256")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.url("inputs", ref.SHA256), nil)
	if err != nil {
		return err
	}
	resp, err := t.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	part := dest + ".part"
	f, err := os.OpenFile(part, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, ref.Size+1))
	closeErr := f.Close()
	switch {
	case copyErr != nil:
		err = copyErr
	case closeErr != nil:
		err = closeErr
	case n != ref.Size:
		err = fmt.Errorf("got %d bytes, expected %d", n, ref.Size)
	case hex.EncodeToString(h.Sum(nil)) != ref.SHA256:
		err = errors.New("content does not match its sha256")
	}
	if err != nil {
		os.Remove(part)
		return err
	}
	return os.Rename(part, dest)
}

// Upload sends src as the declared output name.
func (t *httpTransfer) Upload(ctx context.Context, name, src string) (domain.ArtifactRef, error) {
	f, err := os.Open(src)
	if err != nil {
		return domain.ArtifactRef{}, err
	}
	defer f.Close()
	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return domain.ArtifactRef{}, err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return domain.ArtifactRef{}, err
	}
	var body io.Reader = io.LimitReader(f, size)
	if size == 0 {
		body = http.NoBody // else the client sends "unknown length" and the manager needs one
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, t.url("outputs", name), body)
	if err != nil {
		return domain.ArtifactRef{}, err
	}
	req.ContentLength = size
	req.Header.Set("X-Artifact-SHA256", sum)
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := t.do(req)
	if err != nil {
		return domain.ArtifactRef{}, err
	}
	defer resp.Body.Close()
	var stored struct {
		SHA256 string `json:"sha256"`
		Size   int64  `json:"size"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&stored); err != nil {
		return domain.ArtifactRef{}, fmt.Errorf("decode upload reply: %w", err)
	}
	if stored.SHA256 != sum || stored.Size != size {
		return domain.ArtifactRef{}, fmt.Errorf("manager stored %s (%d bytes), sent %s (%d bytes)", stored.SHA256, stored.Size, sum, size)
	}
	return domain.ArtifactRef{Name: name, SHA256: sum, Size: size}, nil
}

// artifactTransfer builds the transfer for one assignment, reaching the
// manager exactly the way self-update does but with no overall timeout:
// a large file over a slow link can take a while, and the workload's own
// context (canceled with it) bounds the transfer instead.
func (a *Agent) artifactTransfer(wl domain.WorkloadID, token string) (ArtifactTransfer, error) {
	if token == "" {
		return nil, errors.New("the manager sent no transfer token for this workload's files")
	}
	var client *http.Client
	base := a.cfg.SelfUpdateBaseURL
	if a.cfg.SelfUpdateHTTPClient != nil && base != "" {
		c := *a.cfg.SelfUpdateHTTPClient
		c.Timeout = 0
		client = &c
	} else {
		addr := a.getCurrentManagerAddr()
		if addr == "" {
			return nil, errors.New("no known manager address")
		}
		c := *a.selfUpdateHTTPClient()
		c.Timeout = 0
		client = &c
		base = fmt.Sprintf("%s://%s", a.selfUpdateScheme(), addr)
	}
	return &httpTransfer{client: client, base: base, workload: wl, token: token}, nil
}

// defaultWorkRoot is where file workloads' working directories go when
// -work-dir isn't set: the user cache, never the identity directory (a
// workload's own "rm -rf .." must not be able to take the node key).
func defaultWorkRoot(node domain.NodeID) string {
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	return filepath.Join(base, "home-harness", "work", string(node))
}

// within reports whether path is dir or inside it.
func within(path, dir string) bool {
	p, err1 := filepath.Abs(path)
	d, err2 := filepath.Abs(dir)
	if err1 != nil || err2 != nil {
		return false
	}
	rel, err := filepath.Rel(d, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
