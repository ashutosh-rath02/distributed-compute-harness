package manager

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"home-harness/internal/domain"
)

// Named users for the operator API: a role each (roles.go), a token of 256
// random bits of which only the SHA-256 is kept (so a copy of the database
// cannot sign anyone in), optional expiry, and dashboard sessions that die
// when the token is replaced or the user removed.

// userStore is the optional half of the persistent store that keeps users;
// *persistent.Store has it, a nil or foreign store means in-memory only.
type userStore interface {
	PutUser(u domain.User) error
	DeleteUser(name string) error
	ListUsers() ([]domain.User, error)
}

// secretStore keeps a rotated secret across restarts (the pairing token).
type secretStore interface {
	GetSecret(key string) (string, bool, error)
	PutSecret(key, value string) error
}

const (
	userSessionTTL      = 12 * time.Hour
	userLastUsedEvery   = time.Minute
	maxUsers            = 100
	userSessionKeyLabel = "harness-user-session-key-v1"
)

var userNameRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// reservedUserNames are actor strings the audit log already uses; a user
// with one of them could pass for a credential form (or, for "node" and
// "anonymous", be rerouted out of the security log).
var reservedUserNames = map[string]bool{
	actorOperatorToken: true, actorDashboardSession: true, actorAIKey: true, actorUnauthenticated: true,
	actorNode: true, actorAnonymous: true, actorRemote: true, "admin": true, "system": true,
}

var (
	errNoSuchUser   = errors.New("no such user")
	errUserExists   = errors.New("a user with that name already exists")
	errBadUserName  = errors.New("a user name is 1-32 characters: lowercase letters, digits, - and _, starting with a letter")
	errReservedName = errors.New("that name is reserved")
	errTooManyUsers = fmt.Errorf("at most %d users", maxUsers)
)

type userTable struct {
	mu     sync.Mutex
	loaded bool
	users  map[string]*domain.User
	// lastWrite throttles persisting LastUsed.
	lastWrite map[string]time.Time
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func secretHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // the OS has no randomness: nothing safe to do
	}
	return hex.EncodeToString(b)
}

// newUserToken is 256 random bits as 64 hex characters (the form the
// dashboard's login link accepts).
func newUserToken() string { return secretHex(32) }

// load reads stored users once; the caller holds mu.
func (s *Server) loadUsersLocked() {
	t := &s.users
	if t.loaded {
		return
	}
	t.loaded = true
	t.users = map[string]*domain.User{}
	t.lastWrite = map[string]time.Time{}
	if us, ok := s.store.(userStore); ok {
		list, err := us.ListUsers()
		if err != nil {
			log.Printf("manager: load users: %v", err)
		}
		for i := range list {
			u := list[i]
			t.users[u.Name] = &u
		}
	}
}

func (s *Server) saveUser(u domain.User) error {
	if us, ok := s.store.(userStore); ok {
		return us.PutUser(u)
	}
	return nil
}

func validUserName(name string) error {
	if !userNameRE.MatchString(name) {
		return errBadUserName
	}
	if reservedUserNames[name] {
		return errReservedName
	}
	return nil
}

// AddUser makes a user and returns its token, shown to the caller once.
func (s *Server) AddUser(name string, role domain.Role, expires time.Time) (string, error) {
	if err := validUserName(name); err != nil {
		return "", err
	}
	if !role.Valid() {
		return "", errors.New("role must be viewer, operator or admin")
	}
	t := &s.users
	t.mu.Lock()
	defer t.mu.Unlock()
	s.loadUsersLocked()
	if _, ok := t.users[name]; ok {
		return "", errUserExists
	}
	if len(t.users) >= maxUsers {
		return "", errTooManyUsers
	}
	token := newUserToken()
	u := domain.User{Name: name, Role: role, TokenHash: hashToken(token), Epoch: secretHex(8), Created: time.Now().UTC(), Expires: expires}
	if err := s.saveUser(u); err != nil {
		return "", err
	}
	t.users[name] = &u
	return token, nil
}

// RemoveUser deletes a user; its token and every session stop working.
func (s *Server) RemoveUser(name string) error {
	t := &s.users
	t.mu.Lock()
	defer t.mu.Unlock()
	s.loadUsersLocked()
	if _, ok := t.users[name]; !ok {
		return errNoSuchUser
	}
	if us, ok := s.store.(userStore); ok {
		if err := us.DeleteUser(name); err != nil {
			return err
		}
	}
	delete(t.users, name)
	return nil
}

// ReissueUserToken replaces a user's token and returns the new one. With
// grace > 0 the old token keeps working that long (a smooth hand-over);
// either way every dashboard session is signed out.
func (s *Server) ReissueUserToken(name string, grace time.Duration) (string, error) {
	t := &s.users
	t.mu.Lock()
	defer t.mu.Unlock()
	s.loadUsersLocked()
	cur, ok := t.users[name]
	if !ok {
		return "", errNoSuchUser
	}
	token := newUserToken()
	next := *cur
	next.PrevTokenHash, next.PrevExpires = "", time.Time{}
	if grace > 0 {
		next.PrevTokenHash, next.PrevExpires = cur.TokenHash, time.Now().Add(grace).UTC()
	}
	next.TokenHash, next.Epoch = hashToken(token), secretHex(8)
	if err := s.saveUser(next); err != nil {
		return "", err
	}
	*cur = next
	return token, nil
}

// userView is what the API shows of a user: never a token or hash.
type userView struct {
	Name     string      `json:"name"`
	Role     domain.Role `json:"role"`
	Created  time.Time   `json:"created"`
	LastUsed *time.Time  `json:"lastUsed,omitempty"`
	Expires  *time.Time  `json:"expires,omitempty"`
	Expired  bool        `json:"expired,omitempty"`
}

func (s *Server) ListUsers() []userView {
	t := &s.users
	t.mu.Lock()
	defer t.mu.Unlock()
	s.loadUsersLocked()
	out := make([]userView, 0, len(t.users))
	for _, u := range t.users {
		v := userView{Name: u.Name, Role: u.Role, Created: u.Created}
		if !u.LastUsed.IsZero() {
			lu := u.LastUsed
			v.LastUsed = &lu
		}
		if !u.Expires.IsZero() {
			e := u.Expires
			v.Expires, v.Expired = &e, time.Now().After(e)
		}
		out = append(out, v)
	}
	sortUserViews(out)
	return out
}

func sortUserViews(v []userView) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j].Name < v[j-1].Name; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}

// userByToken finds the user a bearer token belongs to. It compares the
// token's hash with every user's in constant time, and always visits all
// of them, so timing says nothing about which user (or whether any).
func (s *Server) userByToken(bearer string, now time.Time) (domain.User, bool) {
	if len(bearer) == 0 || len(bearer) > 256 {
		return domain.User{}, false
	}
	h := []byte(hashToken(bearer))
	t := &s.users
	t.mu.Lock()
	defer t.mu.Unlock()
	s.loadUsersLocked()
	var found *domain.User
	for _, u := range t.users {
		match := subtle.ConstantTimeCompare(h, []byte(u.TokenHash)) == 1
		if u.PrevTokenHash != "" && now.Before(u.PrevExpires) &&
			subtle.ConstantTimeCompare(h, []byte(u.PrevTokenHash)) == 1 {
			match = true
		}
		if match && found == nil {
			found = u
		}
	}
	if found == nil || (!found.Expires.IsZero() && now.After(found.Expires)) {
		return domain.User{}, false
	}
	s.touchLocked(found, now)
	return *found, true
}

// touchLocked records last use at most once a minute per user.
func (s *Server) touchLocked(u *domain.User, now time.Time) {
	t := &s.users
	if now.Sub(t.lastWrite[u.Name]) < userLastUsedEvery {
		return
	}
	t.lastWrite[u.Name] = now
	u.LastUsed = now.UTC()
	if err := s.saveUser(*u); err != nil {
		log.Printf("manager: record last use of %q: %v", u.Name, err)
	}
}

// currentUser returns a user's present record (role changes and removal
// take effect on the next request, whatever a session says).
func (s *Server) currentUser(name string, now time.Time) (domain.User, bool) {
	t := &s.users
	t.mu.Lock()
	defer t.mu.Unlock()
	s.loadUsersLocked()
	u, ok := t.users[name]
	if !ok || (!u.Expires.IsZero() && now.After(u.Expires)) {
		return domain.User{}, false
	}
	return *u, true
}

// ---- dashboard sessions
//
// "s1.<base64url(name|expiry|epoch)>.<hex mac>". The MAC key comes from the
// operator token file, not the database, so a copied database cannot forge a
// session; the epoch changes with the user's token, so a new token or a
// removal signs every browser out; the expiry bounds a leaked one. Sessions
// are stateless, so they survive manager restarts. A session for the legacy
// operator token (admin, no user record) uses the same form with an empty name
// and the token itself as epoch.

const sessionPrefix = "s1."

func sessionKey(operatorToken string) []byte {
	m := hmac.New(sha256.New, []byte(operatorToken))
	m.Write([]byte(userSessionKeyLabel))
	return m.Sum(nil)
}

func signSession(operatorToken, name, epoch string, exp time.Time) string {
	payload := name + "|" + strconv.FormatInt(exp.Unix(), 10) + "|" + epoch
	body := base64.RawURLEncoding.EncodeToString([]byte(payload))
	m := hmac.New(sha256.New, sessionKey(operatorToken))
	m.Write([]byte(body))
	return sessionPrefix + body + "." + hex.EncodeToString(m.Sum(nil))
}

// parseSession verifies a session's MAC and expiry and returns its parts.
func parseSession(operatorToken, value string, now time.Time) (name, epoch string, ok bool) {
	rest, found := strings.CutPrefix(value, sessionPrefix)
	if !found || len(value) > 512 {
		return "", "", false
	}
	body, mac, found := strings.Cut(rest, ".")
	if !found {
		return "", "", false
	}
	m := hmac.New(sha256.New, sessionKey(operatorToken))
	m.Write([]byte(body))
	if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(m.Sum(nil))), []byte(mac)) != 1 {
		return "", "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return "", "", false
	}
	parts := strings.SplitN(string(raw), "|", 3)
	if len(parts) != 3 {
		return "", "", false
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || !now.Before(time.Unix(exp, 0)) {
		return "", "", false
	}
	return parts[0], parts[2], true
}
