package failover

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"home-harness/internal/mtls"
)

// NewClient is an HTTPS client for the standby link, trusting only the
// manager certificate with fingerprint (the pair's shared identity). It
// bounds connecting and waiting for an answer — a sleeping or powered-off
// peer never refuses, it just doesn't answer — but not the transfer
// itself: a database copy can be large.
func NewClient(fingerprint string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig:       mtls.PinnedClientConfig(fingerprint),
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		MaxIdleConns:          2,
		IdleConnTimeout:       time.Minute,
	}}
}

func linkURL(addr, route string) string { return "https://" + addr + route }

// FetchStatus asks the manager at addr (over client, pinned) for its role
// and term, authenticated with the pair secret. An error means no usable
// answer: the peer is down, not listening (a standby doesn't), or refused.
func FetchStatus(ctx context.Context, client *http.Client, addr, secret string) (Status, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, linkURL(addr, RouteStatus), nil)
	if err != nil {
		return Status{}, err
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err := client.Do(req)
	if err != nil {
		return Status{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Status{}, fmt.Errorf("failover: %s answered %s", addr, resp.Status)
	}
	var st Status
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&st); err != nil {
		return Status{}, fmt.Errorf("failover: status from %s: %w", addr, err)
	}
	return st, nil
}

// Enroll trades a one-time standby token for the pair secret.
func Enroll(ctx context.Context, client *http.Client, primary, token string, req EnrollRequest) (EnrollResponse, error) {
	body, _ := json.Marshal(req)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, linkURL(primary, RouteEnroll), strings.NewReader(string(body)))
	if err != nil {
		return EnrollResponse{}, err
	}
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(r)
	if err != nil {
		return EnrollResponse{}, fmt.Errorf("failover: reach the primary at %s: %w", primary, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return EnrollResponse{}, fmt.Errorf("failover: the primary refused the standby token (%s): %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	var out EnrollResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return EnrollResponse{}, err
	}
	if len(out.Secret) < 32 {
		return EnrollResponse{}, fmt.Errorf("failover: the primary sent no usable pair secret")
	}
	return out, nil
}
