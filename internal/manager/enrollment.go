package manager

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"home-harness/internal/joinscript"
)

const defaultEnrollmentTTL = 10 * time.Minute

type enrollment struct {
	Token           string
	PublicURL       string
	ExpiresAt       time.Time
	Platform        string
	Mode            joinscript.Mode
	RelayCredential string
}

type enrollmentStore struct {
	mu      sync.Mutex
	entries map[string]enrollment
}

func newEnrollmentStore() *enrollmentStore {
	return &enrollmentStore{entries: make(map[string]enrollment)}
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (s *enrollmentStore) create(publicURL, platform string, mode joinscript.Mode, ttl time.Duration) (enrollment, error) {
	token, err := randomToken()
	if err != nil {
		return enrollment{}, err
	}
	now := time.Now().UTC()
	e := enrollment{Token: token, PublicURL: publicURL, ExpiresAt: now.Add(ttl), Platform: platform, Mode: mode}
	s.mu.Lock()
	defer s.mu.Unlock()
	for token, old := range s.entries {
		if !old.ExpiresAt.After(now) {
			delete(s.entries, token)
		}
	}
	s.entries[e.Token] = e
	return e, nil
}

func (s *enrollmentStore) remove(token string) {
	s.mu.Lock()
	delete(s.entries, token)
	s.mu.Unlock()
}

func (s *enrollmentStore) get(token string) (enrollment, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[token]
	if !ok {
		return enrollment{}, false
	}
	if !e.ExpiresAt.After(time.Now().UTC()) {
		delete(s.entries, token)
		return enrollment{}, false
	}
	return e, true
}

// consume atomically admits exactly one registration for a token.
func (s *enrollmentStore) consume(token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[token]
	if !ok || !e.ExpiresAt.After(time.Now().UTC()) {
		delete(s.entries, token)
		return false
	}
	delete(s.entries, token)
	return true
}

type createEnrollmentRequest struct {
	Addr     string          `json:"addr"`
	Platform string          `json:"platform"`
	Mode     joinscript.Mode `json:"mode"`
}

type enrollmentView struct {
	Token     string    `json:"token"`
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expiresAt"`
	QRPath    string    `json:"qrPath"`
}

func (s *Server) apiCreateEnrollment(w http.ResponseWriter, r *http.Request) {
	if s.agentBinaryHash == "" {
		http.Error(w, "manager has no agent binary configured", http.StatusConflict)
		return
	}
	var req createEnrollmentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Mode == "" {
		req.Mode = joinscript.ModeLAN
	}
	if req.Mode != joinscript.ModeLAN && req.Mode != joinscript.ModeRemote {
		http.Error(w, "mode must be lan or remote", http.StatusBadRequest)
		return
	}
	if req.Platform != "windows" && req.Platform != "android" {
		http.Error(w, "platform must be windows or android", http.StatusBadRequest)
		return
	}
	if s.agentBinaryOS != "" {
		matches := req.Platform == "windows" && s.agentBinaryOS == "windows" ||
			req.Platform == "android" && s.agentBinaryOS == "linux" && s.agentBinaryArch == "arm64"
		if !matches {
			http.Error(w, fmt.Sprintf("configured agent binary is for %s/%s, not %s", s.agentBinaryOS, s.agentBinaryArch, req.Platform), http.StatusConflict)
			return
		}
	}
	var base string
	if req.Mode == joinscript.ModeRemote {
		if s.cfg.EnrollmentPublisher == nil || s.cfg.RelayPublicURL == "" || s.cfg.RelayAddr == "" {
			http.Error(w, "internet enrollment is not configured on this manager", http.StatusConflict)
			return
		}
		base = strings.TrimRight(s.cfg.RelayPublicURL, "/")
	} else {
		if err := joinscript.ValidateAddress(req.Addr); err != nil {
			http.Error(w, fmt.Sprintf("invalid public manager address %q (want host:port)", req.Addr), http.StatusBadRequest)
			return
		}
		scheme := "https"
		if s.cfg.Fingerprint == "" {
			scheme = "http"
		}
		base = scheme + "://" + req.Addr
	}
	e, err := s.enrollments.create(base, req.Platform, req.Mode, s.cfg.EnrollmentTTL)
	if err != nil {
		http.Error(w, "create enrollment: "+err.Error(), http.StatusInternalServerError)
		return
	}
	e.PublicURL = base + "/enroll/" + e.Token
	s.enrollments.mu.Lock()
	s.enrollments.entries[e.Token] = e
	s.enrollments.mu.Unlock()
	if _, err := qrSVG(e.PublicURL); err != nil {
		s.enrollments.remove(e.Token)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Mode == joinscript.ModeRemote {
		credential, err := s.cfg.EnrollmentPublisher.Publish(r.Context(), PublishedEnrollment{
			Token: e.Token, ExpiresAt: e.ExpiresAt,
			Fingerprint: s.cfg.Fingerprint, Insecure: s.cfg.Fingerprint == "",
			Session: s.cfg.RelayToken,
		})
		if err != nil {
			s.enrollments.remove(e.Token)
			http.Error(w, "publish enrollment to relay: "+err.Error(), http.StatusBadGateway)
			return
		}
		e.RelayCredential = credential
		s.enrollments.mu.Lock()
		s.enrollments.entries[e.Token] = e
		s.enrollments.mu.Unlock()
	}
	writeJSON(w, http.StatusCreated, enrollmentView{Token: e.Token, URL: e.PublicURL, ExpiresAt: e.ExpiresAt, QRPath: "/enrollments/" + e.Token + "/qr"})
}

func (s *Server) apiGetEnrollmentQR(w http.ResponseWriter, r *http.Request) {
	e, ok := s.enrollments.get(r.PathValue("token"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	svg, err := qrSVG(e.PublicURL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(svg)
}

var enrollmentPage = template.Must(template.New("enrollment").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Join Home Compute Harness</title><style>body{font-family:system-ui,sans-serif;max-width:42rem;margin:3rem auto;padding:0 1rem}a{display:block;margin:1rem 0;padding:1rem;background:#2f81f7;color:white;text-decoration:none;border-radius:.5rem}.note{color:#666}</style></head>
<body><h1>Join Home Compute Harness</h1><p>This single-use {{.Platform}} invitation expires at {{.Expires}}.</p>
<a href="{{.Base}}/setup">Open setup instructions</a>
<p class="note">Copy and run the displayed command in this device's terminal. Your browser may show a certificate warning because this local manager uses its own pinned certificate; the setup script independently verifies the downloaded binary hash.</p></body></html>`))

// EnrollmentHandler is deliberately mounted only on the agent-facing
// transport. It exposes token-scoped bootstrap resources, never the
// privileged operator API.
func (s *Server) EnrollmentHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		path := strings.TrimPrefix(r.URL.Path, "/enroll/")
		parts := strings.Split(path, "/")
		if len(parts) == 0 || parts[0] == "" {
			http.NotFound(w, r)
			return
		}
		e, ok := s.enrollments.get(parts[0])
		if !ok {
			http.NotFound(w, r)
			return
		}
		basePath := "/enroll/" + e.Token
		if len(parts) == 1 {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			enrollmentPage.Execute(w, map[string]string{"Expires": e.ExpiresAt.Format(time.RFC3339), "Base": basePath, "Platform": e.Platform})
			return
		}
		switch parts[1] {
		case "agent-binary":
			s.AgentBinaryHandler()(w, r)
		case "setup":
			publicURL, err := url.Parse(e.PublicURL)
			if err != nil || publicURL.Host == "" {
				http.Error(w, "invalid enrollment URL", http.StatusInternalServerError)
				return
			}
			info := joinscript.Info{
				Fingerprint: s.cfg.Fingerprint, PairingToken: e.Token,
				Insecure: s.cfg.Fingerprint == "", AgentBinaryAvailable: true,
				AgentBinarySHA256: s.agentBinaryHash, AgentBinaryPath: basePath + "/agent-binary",
			}
			var script string
			if e.Mode == joinscript.ModeRemote {
				info.RelayAvailable, info.RelayAddr, info.RelayToken = true, s.cfg.RelayAddr, e.RelayCredential
				info.BootstrapURL = e.PublicURL + "/agent-binary"
				script, err = joinscript.BuildMode(joinscript.ModeRemote, "", e.Platform, info)
			} else {
				script, err = joinscript.Build(publicURL.Host, e.Platform, info)
			}
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Write([]byte(script))
		default:
			http.NotFound(w, r)
		}
	}
}
