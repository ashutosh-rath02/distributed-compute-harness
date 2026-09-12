package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"home-harness/internal/mtls"
)

const (
	selfUpdateDownloadSuffix = ".download"
	selfUpdateOldSuffix      = ".old"
	selfUpdateAgentLogFile   = "agent.log"
)

// hashFile returns the hex-encoded SHA-256 of path's content — the same
// encoding convention as mtls.Fingerprint/identity.DeriveNodeID (the only
// existing sha256 users in this codebase), extended here to a file's
// content rather than an in-memory byte slice.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// removeStaleUpdateFile best-effort removes a ".old" file left over from a
// previous self-update's swap (see applySelfUpdate) — non-fatal if it's
// still transiently locked; it self-heals on the next successful startup.
func removeStaleUpdateFile(exePath string) {
	if err := os.Remove(exePath + selfUpdateOldSuffix); err != nil && !os.IsNotExist(err) {
		log.Printf("agent: could not remove stale %s: %v", exePath+selfUpdateOldSuffix, err)
	}
}

// selfUpdateHTTPClient builds the client performSelfUpdate downloads with,
// pinned to the manager's certificate fingerprint exactly like the main WS
// transport already is (mtls.PinnedClientConfig) — not a new trust
// mechanism, reused for this second, short-lived connection.
func (a *Agent) selfUpdateHTTPClient() *http.Client {
	if a.cfg.Insecure {
		return http.DefaultClient
	}
	return &http.Client{
		Timeout:   2 * time.Minute,
		Transport: &http.Transport{TLSClientConfig: mtls.PinnedClientConfig(a.cfg.ManagerFingerprint)},
	}
}

func (a *Agent) selfUpdateScheme() string {
	if a.cfg.Insecure {
		return "http"
	}
	return "https"
}

// performSelfUpdate downloads, verifies, and swaps in a new agent binary,
// then relaunches with the exact flags this process was started with. Any
// failure before the swap (download error, hash mismatch) leaves the
// running process completely untouched; see applySelfUpdate for the swap
// step's own failure handling. Called from a goroutine spawned by
// handleCommand only after the SELF_UPDATE command's ack has already been
// sent — see commands.go.
func (a *Agent) performSelfUpdate(expectedSHA256 string) {
	exePath, err := os.Executable()
	if err != nil {
		log.Printf("agent %s: self-update: could not determine own executable path: %v", a.identity.NodeID, err)
		return
	}
	a.performSelfUpdateAt(exePath, expectedSHA256)
}

// performSelfUpdateAt is performSelfUpdate with the executable path
// injected, so tests can exercise the full download/verify/swap flow
// against a throwaway temp file standing in for the running binary,
// without ever touching the actual test binary on disk.
func (a *Agent) performSelfUpdateAt(exePath, expectedSHA256 string) {
	addr := a.getCurrentManagerAddr()
	if addr == "" {
		log.Printf("agent %s: self-update: no known manager address, aborting", a.identity.NodeID)
		return
	}
	downloadPath := exePath + selfUpdateDownloadSuffix

	url := fmt.Sprintf("%s://%s/agent-binary", a.selfUpdateScheme(), addr)
	if err := downloadFile(a.selfUpdateHTTPClient(), url, downloadPath); err != nil {
		log.Printf("agent %s: self-update: download failed: %v", a.identity.NodeID, err)
		os.Remove(downloadPath)
		return
	}

	got, err := hashFile(downloadPath)
	if err != nil {
		log.Printf("agent %s: self-update: could not hash downloaded file: %v", a.identity.NodeID, err)
		os.Remove(downloadPath)
		return
	}
	if got != expectedSHA256 {
		log.Printf("agent %s: self-update: hash mismatch (got %s, want %s), discarding download", a.identity.NodeID, got, expectedSHA256)
		os.Remove(downloadPath)
		return
	}

	if err := applySelfUpdate(exePath, downloadPath); err != nil {
		log.Printf("agent %s: self-update: could not apply update, remaining on current binary: %v", a.identity.NodeID, err)
		return
	}

	log.Printf("agent %s: self-update: relaunching with new binary", a.identity.NodeID)
	relaunch(exePath, a.cfg.LaunchArgs, filepath.Join(a.cfg.IdentityDir, selfUpdateAgentLogFile))
}

// downloadFile streams url's body to destPath, so a large file never sits
// fully in memory before being written. Created with the executable bit
// set (0o755, not os.Create's default 0o666-minus-umask): confirmed via
// real hardware (an Android/Termux node) that a downloaded binary lacking
// +x is silently irrelevant on Windows (no POSIX exec bit) but fails every
// subsequent exec attempt on Linux with a plain "permission denied" — not
// an OS security restriction, just a missing chmod this v4 self-update
// path never needed until a non-Windows node exercised it.
func downloadFile(client *http.Client, url, destPath string) error {
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %s", resp.Status)
	}
	out, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, resp.Body)
	return err
}

// applySelfUpdate swaps downloadPath into place at exePath, retrying the
// final rename to absorb a transient lock (e.g. Windows Defender's
// real-time scan of a freshly-written, unsigned .exe) rather than failing
// outright on the first attempt. It leaves exePath pointing at a
// fully-functional binary on every return: the new one on success, or the
// restored old one if every retry is exhausted — never neither.
func applySelfUpdate(exePath, downloadPath string) error {
	oldPath := exePath + selfUpdateOldSuffix
	// Windows permits renaming a running/open executable (just not
	// deleting or overwriting it in place) — the same mechanism real-world
	// Windows updaters use to replace themselves.
	if err := os.Rename(exePath, oldPath); err != nil {
		os.Remove(downloadPath)
		return fmt.Errorf("rename current binary out of the way: %w", err)
	}

	var renameErr error
	backoff := 200 * time.Millisecond
	for attempt := 0; attempt < 5; attempt++ {
		if renameErr = os.Rename(downloadPath, exePath); renameErr == nil {
			os.Remove(oldPath) // best-effort; may still be transiently locked, self-heals at next startup
			return nil
		}
		time.Sleep(backoff)
		backoff *= 2
	}

	if rollbackErr := os.Rename(oldPath, exePath); rollbackErr != nil {
		return fmt.Errorf("rename new binary into place failed (%v), AND rollback failed (%v) — manual recovery needed from %s", renameErr, rollbackErr, oldPath)
	}
	os.Remove(downloadPath)
	return fmt.Errorf("rename new binary into place failed after retries (likely a transient lock, e.g. antivirus scanning the new file), rolled back to the previous binary: %w", renameErr)
}

// relaunch execs exePath with args as a new, detached process — Windows
// does not tie a child's lifetime to its parent's the way a POSIX process
// group can, so a plain Start() is sufficient — with its output redirected
// to logPath rather than left nil, which would otherwise silently discard
// it to the null device on Windows: exactly the kind of silent failure
// this feature exists to avoid. A package-level var so tests can
// substitute a no-op/observable stub instead of actually exec'ing and
// exiting the test process.
var relaunch = func(exePath string, args []string, logPath string) {
	cmd := exec.Command(exePath, args...)
	if logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	} else {
		log.Printf("agent: self-update: could not open log file %s for relaunched process: %v", logPath, err)
	}
	if err := cmd.Start(); err != nil {
		log.Printf("agent: self-update: relaunch failed: %v", err)
		return
	}
	os.Exit(0)
}
