package manager

import (
	"crypto/subtle"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"home-harness/internal/domain"
)

// The three shared secrets (operator token, AI key, pairing token) can be
// rotated while the manager runs, so every reader goes through these
// accessors instead of cfg. cfg keeps the values the manager started with.
//
//	operator token  every client holding it (harnessctl, scripts) and every
//	                dashboard session stop working; users' own tokens do not.
//	AI key          apps using the OpenAI-compatible API need the new key.
//	pairing token   new devices can no longer join with the old one; devices
//	                already admitted keep working (they reconnect with their
//	                own key, not the pairing token). Invitations are unaffected.
type secretState struct {
	mu       sync.RWMutex
	operator string
	ai       string
	pairing  string
	once     sync.Once
}

func (s *Server) operatorToken() string {
	s.secrets.mu.RLock()
	defer s.secrets.mu.RUnlock()
	if s.secrets.operator != "" {
		return s.secrets.operator
	}
	return s.cfg.OperatorToken
}

func (s *Server) aiKey() string {
	s.secrets.mu.RLock()
	defer s.secrets.mu.RUnlock()
	if s.secrets.ai != "" {
		return s.secrets.ai
	}
	return s.cfg.AIKey
}

const pairingSecretKey = "pairing-token"

// pairingToken is the current shared pairing token. A rotated one is kept
// in the database and wins over the -pairing-token the manager was started
// with (a launcher keeps passing the old flag value until its file is
// re-read); rotating again is how to change it.
func (s *Server) pairingToken() string {
	s.secrets.once.Do(func() {
		if ss, ok := s.store.(secretStore); ok {
			if v, found, err := ss.GetSecret(pairingSecretKey); err != nil {
				log.Printf("manager: read rotated pairing token: %v", err)
			} else if found && v != "" {
				s.secrets.mu.Lock()
				s.secrets.pairing = v
				s.secrets.mu.Unlock()
			}
		}
	})
	s.secrets.mu.RLock()
	defer s.secrets.mu.RUnlock()
	if s.secrets.pairing != "" {
		return s.secrets.pairing
	}
	return s.cfg.PairingToken
}

// pairingMatches compares a presented pairing token in constant time; an
// empty configured token matches nothing.
func (s *Server) pairingMatches(presented string) bool {
	want := s.pairingToken()
	return want != "" && subtle.ConstantTimeCompare([]byte(presented), []byte(want)) == 1
}

// writeSecretFile replaces path atomically with value, owner-only.
func writeSecretFile(path, value string) error {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".secret-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if err := os.Chmod(name, 0o600); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if _, err := tmp.WriteString(value + "\n"); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// apiRotate: POST /rotate/{what} (operator-token, ai-key or pairing-token)
// makes a new random secret, saves it where the launchers read it, and
// returns it once. Nothing else needs a restart.
func (s *Server) apiRotate(w http.ResponseWriter, r *http.Request) {
	what := r.PathValue("what")
	fresh := newUserToken()
	var file string
	switch what {
	case "operator-token":
		file = s.cfg.OperatorTokenFile
	case "ai-key":
		file = s.cfg.AIKeyFile
	case "pairing-token":
		file = s.cfg.PairingTokenFile
	default:
		http.Error(w, "rotate operator-token, ai-key or pairing-token", http.StatusNotFound)
		return
	}
	if what == "operator-token" && file == "" {
		http.Error(w, "this manager keeps its operator token in memory only; restart it with a new -operator-token-file instead", http.StatusConflict)
		return
	}
	if what == "pairing-token" {
		ss, ok := s.store.(secretStore)
		if !ok {
			http.Error(w, "this manager has no database to remember a new pairing token in", http.StatusConflict)
			return
		}
		if err := ss.PutSecret(pairingSecretKey, fresh); err != nil {
			http.Error(w, "could not save the new pairing token: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if file != "" {
		if err := writeSecretFile(file, fresh); err != nil {
			http.Error(w, "could not write the new "+what+": "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	s.pairingToken() // load any stored value first so it cannot overwrite this one
	s.secrets.mu.Lock()
	switch what {
	case "operator-token":
		s.secrets.operator = fresh
	case "ai-key":
		s.secrets.ai = fresh
	case "pairing-token":
		s.secrets.pairing = fresh
	}
	s.secrets.mu.Unlock()
	s.audit(domain.AuditSecurity, "secret.rotated", "", actorFrom(r.Context()), map[string]any{"what": what})
	resp := map[string]any{"rotated": what, "value": fresh}
	if file != "" {
		resp["file"] = file
	}
	writeJSON(w, http.StatusOK, resp)
}

// ---- failed sign-in attempts

const (
	authFailLimit  = 10
	authFailWindow = time.Minute
	authFailKeys   = 512
)

// authLimiter counts failed credentials per source, in a bounded map. It
// only ever slows invalid attempts down: a valid credential is checked
// first and never refused, because on a phone every app shares 127.0.0.1
// and a hostile one must not be able to lock the owner out.
type authLimiter struct {
	mu      sync.Mutex
	entries map[string]*authFailEntry
}

type authFailEntry struct {
	start time.Time
	n     int
}

// tooMany records one failure from source and reports whether it is over
// budget (more than authFailLimit failures in the current window).
func (l *authLimiter) tooMany(source string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.entries == nil {
		l.entries = make(map[string]*authFailEntry)
	}
	e := l.entries[source]
	if e == nil || now.Sub(e.start) >= authFailWindow {
		if e == nil && len(l.entries) >= authFailKeys {
			for k, v := range l.entries {
				if now.Sub(v.start) >= authFailWindow {
					delete(l.entries, k)
				}
			}
			if len(l.entries) >= authFailKeys {
				// Full of live entries: forget them all rather than grow.
				l.entries = make(map[string]*authFailEntry)
			}
		}
		e = &authFailEntry{start: now}
		l.entries[source] = e
	}
	e.n++
	return e.n > authFailLimit
}

// ---- user management API

type addUserRequest struct {
	Name        string      `json:"name"`
	Role        domain.Role `json:"role"`
	ExpiresDays int         `json:"expiresDays"`
}

func (s *Server) apiListUsers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.ListUsers())
}

func userError(w http.ResponseWriter, err error) {
	code := http.StatusBadRequest
	switch err {
	case errNoSuchUser:
		code = http.StatusNotFound
	case errUserExists:
		code = http.StatusConflict
	}
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func (s *Server) apiAddUser(w http.ResponseWriter, r *http.Request) {
	var req addUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var exp time.Time
	if req.ExpiresDays < 0 || req.ExpiresDays > 3650 {
		http.Error(w, "expiresDays must be between 0 (never) and 3650", http.StatusBadRequest)
		return
	}
	if req.ExpiresDays > 0 {
		exp = time.Now().AddDate(0, 0, req.ExpiresDays).UTC()
	}
	token, err := s.AddUser(req.Name, req.Role, exp)
	if err != nil {
		userError(w, err)
		return
	}
	s.audit(domain.AuditSecurity, "user.added", "", actorFrom(r.Context()), map[string]any{"user": req.Name, "role": string(req.Role)})
	writeJSON(w, http.StatusCreated, map[string]any{"name": req.Name, "role": req.Role, "token": token})
}

func (s *Server) apiRemoveUser(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.RemoveUser(name); err != nil {
		userError(w, err)
		return
	}
	s.audit(domain.AuditSecurity, "user.removed", "", actorFrom(r.Context()), map[string]any{"user": clip(name)})
	writeJSON(w, http.StatusOK, map[string]string{"removed": name})
}

// apiReissueToken serves both POST /users/{name}/token (the old token stops
// at once) and /users/{name}/rotate (the old one lives on for ?graceMinutes).
func (s *Server) apiReissueToken(graceful bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		var grace time.Duration
		if graceful {
			grace = 10 * time.Minute
			var req struct {
				GraceMinutes *int `json:"graceMinutes"`
			}
			if json.NewDecoder(r.Body).Decode(&req) == nil && req.GraceMinutes != nil && *req.GraceMinutes >= 0 && *req.GraceMinutes <= 1440 {
				grace = time.Duration(*req.GraceMinutes) * time.Minute
			}
		}
		token, err := s.ReissueUserToken(name, grace)
		if err != nil {
			userError(w, err)
			return
		}
		kind := "user.token-issued"
		if graceful {
			kind = "user.token-rotated"
		}
		s.audit(domain.AuditSecurity, kind, "", actorFrom(r.Context()), map[string]any{"user": clip(name), "graceMinutes": int(grace / time.Minute)})
		writeJSON(w, http.StatusOK, map[string]any{"name": name, "token": token, "graceMinutes": int(grace / time.Minute)})
	}
}

func (s *Server) registerUserRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /whoami", s.apiWhoAmI)
	mux.HandleFunc("GET /users", s.apiListUsers)
	mux.HandleFunc("POST /users", s.apiAddUser)
	mux.HandleFunc("DELETE /users/{name}", s.apiRemoveUser)
	mux.HandleFunc("POST /users/{name}/token", s.apiReissueToken(false))
	mux.HandleFunc("POST /users/{name}/rotate", s.apiReissueToken(true))
	mux.HandleFunc("POST /rotate/{what}", s.apiRotate)
}


