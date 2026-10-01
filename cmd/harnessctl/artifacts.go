package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"home-harness/internal/domain"
)

// Workload files: the manager's artifact store (internal/manager/artifacts.go).

// fileList is a repeatable string flag (-in, -out).
type fileList []string

func (f *fileList) String() string     { return strings.Join(*f, ",") }
func (f *fileList) Set(v string) error { *f = append(*f, v); return nil }

// resolveInputs turns -in values into workload inputs, uploading local
// files first. Each is "name=path", "path" (named after its base name), or
// "name=sha256:<hex>" for a file already stored on the manager.
func (c *apiClient) resolveInputs(specs []string) ([]map[string]string, error) {
	var out []map[string]string
	for _, spec := range specs {
		name, src, named := strings.Cut(spec, "=")
		if !named {
			src, name = spec, filepath.Base(spec)
		}
		if sha, ok := strings.CutPrefix(src, "sha256:"); ok {
			out = append(out, map[string]string{"name": name, "sha256": sha})
			continue
		}
		info, err := c.uploadFile(src)
		if err != nil {
			return nil, fmt.Errorf("-in %s: %w", spec, err)
		}
		out = append(out, map[string]string{"name": name, "sha256": info.SHA256})
	}
	return out, nil
}

type artifactInfo struct {
	SHA256  string    `json:"sha256"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"modTime"`
}

func (c *apiClient) uploadFile(path string) (artifactInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return artifactInfo{}, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return artifactInfo{}, err
	}
	req, err := http.NewRequest(http.MethodPost, c.base+"/artifacts", f)
	if err != nil {
		return artifactInfo{}, err
	}
	req.ContentLength = fi.Size()
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		return artifactInfo{}, fmt.Errorf("upload: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return artifactInfo{}, fmt.Errorf("manager returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var info artifactInfo
	return info, json.NewDecoder(resp.Body).Decode(&info)
}

// cmdArtifact: artifact put <file> | get <sha256> [out-file] | rm <sha256>
func cmdArtifact(c *apiClient, args []string) error {
	if len(args) < 2 {
		return errors.New("usage: harnessctl artifact put <file> | get <sha256> [out-file] | rm <sha256>")
	}
	switch args[0] {
	case "put":
		info, err := c.uploadFile(args[1])
		if err != nil {
			return err
		}
		fmt.Printf("Stored %s (%s)\n", info.SHA256, humanBytes(uint64(info.Size)))
		fmt.Printf("Use it with: harnessctl run -in %s=sha256:%s ...\n", filepath.Base(args[1]), info.SHA256)
		return nil
	case "get":
		out := args[1]
		if len(args) >= 3 {
			out = args[2]
		}
		n, err := c.download(args[1], "", out)
		if err != nil {
			return err
		}
		fmt.Printf("Saved %s (%s)\n", out, humanBytes(uint64(n)))
		return nil
	case "rm":
		req, err := http.NewRequest(http.MethodDelete, c.base+"/artifacts/"+args[1], nil)
		if err != nil {
			return err
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			return fmt.Errorf("manager returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
		}
		fmt.Println("Deleted.")
		return nil
	}
	return fmt.Errorf("unknown artifact subcommand %q", args[0])
}

// download saves artifact sha to path (written to a temp name first).
func (c *apiClient) download(sha, name, path string) (int64, error) {
	url := c.base + "/artifacts/" + sha
	if name != "" {
		url += "?name=" + name
	}
	resp, err := c.http.Get(url)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return 0, fmt.Errorf("manager returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	part := path + ".part"
	f, err := os.Create(part)
	if err != nil {
		return 0, err
	}
	n, copyErr := io.Copy(f, resp.Body)
	closeErr := f.Close()
	if copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		os.Remove(part)
		return 0, copyErr
	}
	return n, os.Rename(part, path)
}

func (c *apiClient) cmdArtifacts() error {
	var list struct {
		Artifacts []artifactInfo `json:"artifacts"`
		Used      int64          `json:"usedBytes"`
		Total     int64          `json:"totalBytes"`
		Max       int64          `json:"maxBytes"`
	}
	if err := c.get("/artifacts", &list); err != nil {
		return err
	}
	fmt.Printf("%d file(s), %s of %s used (largest allowed: %s)\n", len(list.Artifacts), humanBytes(uint64(list.Used)), humanBytes(uint64(list.Total)), humanBytes(uint64(list.Max)))
	if len(list.Artifacts) == 0 {
		return nil
	}
	fmt.Printf("%-66s %-10s %s\n", "SHA256", "SIZE", "LAST USED")
	for _, a := range list.Artifacts {
		fmt.Printf("%-66s %-10s %s\n", a.SHA256, humanBytes(uint64(a.Size)), a.ModTime.Local().Format(time.DateTime))
	}
	return nil
}

// cmdOutputs downloads every output a workload delivered into dir,
// keeping their declared relative names.
func cmdOutputs(c *apiClient, args []string) error {
	fs := flag.NewFlagSet("outputs", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return errors.New("usage: harnessctl outputs <workload-id> [dir]")
	}
	dir := "."
	if fs.NArg() >= 2 {
		dir = fs.Arg(1)
	}
	var w workloadView
	if err := c.get("/workloads/"+fs.Arg(0), &w); err != nil {
		return err
	}
	if len(w.OutputFiles) == 0 {
		return fmt.Errorf("workload %s has no delivered outputs (state %s)", w.ID, w.State)
	}
	for _, o := range w.OutputFiles {
		// The manager validated these names, but they still come from
		// the network: never let one escape dir.
		if err := domain.ValidArtifactName(o.Name); err != nil {
			return err
		}
		dest := filepath.Join(dir, filepath.FromSlash(o.Name))
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		n, err := c.download(o.SHA256, o.Name, dest)
		if err != nil {
			return fmt.Errorf("%s: %w", o.Name, err)
		}
		fmt.Printf("%s (%s)\n", dest, humanBytes(uint64(n)))
	}
	return nil
}
