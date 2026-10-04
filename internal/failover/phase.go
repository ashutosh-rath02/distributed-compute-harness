package failover

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"

	"home-harness/internal/domain"
	"home-harness/internal/mtls"
	"home-harness/internal/store/persistent"
)

// PhaseOptions is what Phase needs from the manager's command line.
type PhaseOptions struct {
	Options
	// Primary, Token and Fingerprint come from -standby-of, -standby-token
	// and -standby-fingerprint: used to enroll only when this manager has
	// no role file yet.
	Primary, Token, Fingerprint string
	// Force lets enrolling replace a database or TLS identity already here.
	Force bool
	// API serves this manager's operator API while it is a standby
	// (GET /standby, POST /standby/promote) and returns how to stop it.
	API func(*Replica) (stop func())
}

// Outcome is how Phase ended: this manager is to serve agents now.
type Outcome struct {
	// PairingToken is the fleet's pairing token when this state was copied
	// from a primary; it wins over the manager's own -pairing-token.
	PairingToken string
	// Promoted: it was a standby during this Phase and was promoted.
	Promoted bool
}

// Phase decides at start whether this manager may serve agents and, while
// it may not, runs it as a standby. It returns once this manager is the
// active one: at once for an ordinary manager, or after a promotion. It
// also makes an active manager that has a standby ask it first, before it
// listens for anyone: if the standby took over meanwhile (a higher term),
// this manager steps down and becomes its standby instead of serving.
func Phase(ctx context.Context, opt PhaseOptions) (Outcome, error) {
	role, err := LoadRole(opt.DBPath)
	if err != nil {
		return Outcome{}, err
	}
	switch {
	case role == nil && opt.Primary != "":
		if role, err = enroll(ctx, opt); err != nil {
			return Outcome{}, err
		}
	case role != nil && role.Role == RoleActive && opt.Primary != "":
		log.Printf("manager: ignoring -standby-of: this manager was promoted (term %d) and is the active one; to make it a standby again, delete %s and enroll it anew", role.SeenTerm, RolePath(opt.DBPath))
	}
	if role == nil || role.Role == RoleActive {
		fenced, err := checkPeerAtStart(ctx, opt.DBPath, opt.TLSDir)
		if err != nil {
			return Outcome{}, err
		}
		if fenced == nil {
			out := Outcome{}
			if role != nil {
				out.PairingToken = role.PairingToken
			}
			return out, nil
		}
		role = fenced
	}
	r := NewReplica(opt.Options, *role)
	stop := func() {}
	if opt.API != nil {
		stop = opt.API(r)
	}
	promoted, err := r.Run(ctx)
	stop()
	if !promoted {
		return Outcome{}, err
	}
	r.mu.Lock()
	token := r.role.PairingToken
	r.mu.Unlock()
	return Outcome{Promoted: true, PairingToken: token}, nil
}

// enroll trades the one-time token for the pair secret and writes the
// role file. Becoming a standby replaces this manager's database and
// identity with the primary's, so an existing one is kept unless forced.
func enroll(ctx context.Context, opt PhaseOptions) (*Role, error) {
	if opt.Token == "" || opt.Fingerprint == "" {
		return nil, errors.New("-standby-of needs -standby-token and -standby-fingerprint (harnessctl standby add on the primary prints them)")
	}
	if !opt.Force {
		for _, p := range []string{opt.DBPath, filepath.Join(opt.TLSDir, "manager-key.pem")} {
			if _, err := os.Stat(p); err == nil {
				return nil, fmt.Errorf("%s already exists: becoming a standby replaces this manager's database and identity with the primary's. Pass -force to do that", p)
			} else if !errors.Is(err, fs.ErrNotExist) {
				return nil, err
			}
		}
	}
	fp := strings.ToLower(strings.TrimSpace(opt.Fingerprint))
	name := opt.Name
	if name == "" {
		name, _ = os.Hostname()
	}
	if opt.Addr == "" {
		log.Printf("manager: standby: no -advertise-addr: devices won't know where to find this manager if it takes over")
	}
	resp, err := Enroll(ctx, NewClient(fp), opt.Primary, opt.Token, EnrollRequest{Name: name, Addr: opt.Addr})
	if err != nil {
		return nil, err
	}
	role := &Role{Role: RoleStandby, Primary: opt.Primary, Fingerprint: fp, Secret: resp.Secret, SeenTerm: resp.Term}
	if err := SaveRole(opt.DBPath, role); err != nil {
		return nil, err
	}
	log.Printf("manager: enrolled as the standby of %s", opt.Primary)
	return role, nil
}

// checkPeerAtStart asks an active manager's standby, before this manager
// serves anyone, whether it has taken over. A peer that doesn't answer is
// not active (a standby doesn't listen at all): this manager serves, and
// keeps asking every few seconds while it does (manager.watchPeer).
func checkPeerAtStart(ctx context.Context, dbPath, tlsDir string) (*Role, error) {
	if _, err := os.Stat(dbPath); err != nil {
		return nil, nil
	}
	cert, err := mtls.Load(tlsDir)
	if err != nil {
		return nil, nil // no identity yet: nobody can be pinned to it
	}
	store, err := persistent.Open(dbPath)
	if err != nil {
		return nil, err
	}
	term, err := store.GetTerm()
	pair, found, perr := store.GetStandbyPair()
	store.Close()
	if err != nil || perr != nil {
		return nil, errors.Join(err, perr)
	}
	if !found || pair.PeerAddr == "" {
		return nil, nil
	}
	fp := mtls.Fingerprint(cert)
	st, err := FetchStatus(ctx, NewClient(fp), pair.PeerAddr, pair.Secret)
	if err != nil {
		log.Printf("manager: the standby at %s isn't serving (%v): this manager serves", pair.PeerAddr, err)
		return nil, nil
	}
	if !Supersedes(cert, term, st) {
		return nil, nil
	}
	log.Printf("manager: the standby at %s took over (term %d, this manager's is %d): stepping down to be its standby", pair.PeerAddr, st.Term, term)
	return StepDown(dbPath, fp, pair, term, st.Term)
}

// Supersedes reports whether a peer's answer proves a newer active
// manager: active, at a higher term than own, with a valid proof by the
// pair's key.
func Supersedes(cert tls.Certificate, own uint64, st Status) bool {
	return st.Role == RoleActive && st.Term > own && VerifyTerm(cert, st.Term, st.TermProof)
}

// StepDown writes the role file of a manager stepping down: a standby of
// pair's peer, the one at newTerm. Its own state is stale from now on
// (CopiedAt zero), so it can't be promoted until it has copied the
// active one's.
func StepDown(dbPath, fingerprint string, pair domain.StandbyPair, own, newTerm uint64) (*Role, error) {
	role := &Role{Role: RoleStandby, Primary: pair.PeerAddr, Fingerprint: fingerprint, Secret: pair.Secret, SeenTerm: newTerm, SteppedDown: true, SteppedDownFrom: own}
	if old, err := LoadRole(dbPath); err == nil && old != nil {
		role.PairingToken = old.PairingToken
		role.SeenTerm = max(role.SeenTerm, old.SeenTerm)
	}
	return role, SaveRole(dbPath, role)
}
