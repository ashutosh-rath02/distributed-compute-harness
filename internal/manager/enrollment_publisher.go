package manager

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type PublishedEnrollment struct {
	Token       string    `json:"token"`
	ExpiresAt   time.Time `json:"expiresAt"`
	Fingerprint string    `json:"fingerprint"`
	Insecure    bool      `json:"insecure"`
	Session     string    `json:"session"`
}

type EnrollmentPublisher interface {
	Publish(context.Context, PublishedEnrollment) (string, error)
}

type HTTPEnrollmentPublisher struct {
	URL, RelaySession, PublishToken string
	Client                          *http.Client
}

func (p *HTTPEnrollmentPublisher) Publish(ctx context.Context, enrollment PublishedEnrollment) (string, error) {
	if enrollment.Session == "" {
		enrollment.Session = p.RelaySession
	}
	body, err := json.Marshal(enrollment)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(p.URL, "/")+"/_harness/enrollments", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.PublishToken)
	client := p.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("relay returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var result struct {
		RelayCredential string `json:"relayCredential"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	if result.RelayCredential == "" {
		return "", fmt.Errorf("relay returned an empty device credential")
	}
	return result.RelayCredential, nil
}
