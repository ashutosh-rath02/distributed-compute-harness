package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"home-harness/internal/artifacts"
	"home-harness/internal/domain"
	"home-harness/internal/failover"
	"home-harness/internal/manager"
)

// A standby manager (roadmap item 17; the fencing rule is in
// internal/failover). A manager started with -standby-of — or one whose
// standby.json beside -db says it is a standby — copies the active
// manager's state and serves only "standby status" and "promote" on its
// operator API until promoted; then it carries on in this same process as
// an ordinary manager, from the copied state. An active manager that
// learns of a newer one stops serving and continues, in this same
// process, as that manager's standby.

type standbyFlags struct {
	of, token, fingerprint *string
	interval, autoPromote  *time.Duration
}

func registerStandbyFlags() *standbyFlags {
	return &standbyFlags{
		of:          flag.String("standby-of", "", "make this manager the standby of the manager at this address (host:port, its agent port): it copies that manager's state — database, TLS identity, tokens, files — and serves no devices until promoted. \"harnessctl standby add\" on the active manager prints the whole command. Only needed once: the role is kept in standby.json beside -db"),
		token:       flag.String("standby-token", "", "the one-time standby token from \"harnessctl standby add\" (with -standby-of)"),
		fingerprint: flag.String("standby-fingerprint", "", "the active manager's TLS fingerprint, pinned on the standby link (with -standby-of)"),
		interval:    flag.Duration("standby-interval", failover.DefaultInterval, "how often a standby checks the active manager for changes"),
		autoPromote: flag.Duration("standby-auto-promote", 0, "a standby promotes itself once the active manager hasn't answered for this long (e.g. 5m); 0 = only by hand (\"harnessctl standby promote\" on the standby). It can't tell a dead manager from a broken network between the two, which is why it is off by default"),
	}
}

// configured reports whether this manager starts as a standby or may
// become one (it then gets its pairing token from the primary).
func (f *standbyFlags) configured(dbPath string) bool {
	if *f.of != "" {
		return true
	}
	role, _ := failover.LoadRole(dbPath)
	return role != nil
}

// standbySetup is what the standby phase needs from main's flags.
type standbySetup struct {
	flags                                  *standbyFlags
	db, tlsDir, aiKeyFile, apiAddr, advert string
	operatorToken                          string
	force                                  bool
	artifactDir, artifactMax, artifactSize string
}

// run is the standby phase (failover.Phase) before the manager serves:
// it returns at once for an ordinary manager, or after this standby's
// promotion. ok is false when the manager should just exit (stopped while
// a standby).
func (s standbySetup) run() (failover.Outcome, bool) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	opt := failover.PhaseOptions{
		Options: failover.Options{
			DBPath: s.db, TLSDir: s.tlsDir, AIKeyFile: s.aiKeyFile,
			Interval: *s.flags.interval, AutoPromoteAfter: *s.flags.autoPromote, Addr: s.advert,
		},
		Primary: *s.flags.of, Token: *s.flags.token, Fingerprint: *s.flags.fingerprint, Force: s.force,
		API: s.serveStandbyAPI,
	}
	if s.flags.configured(s.db) {
		opt.Artifacts = s.openArtifacts()
	}
	out, err := failover.Phase(ctx, opt)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return out, false
		}
		log.Fatalf("manager: %v", err)
	}
	return out, true
}

// openArtifacts opens the file store a standby mirrors into (the same one
// it serves from once promoted); nil leaves files out.
func (s standbySetup) openArtifacts() *artifacts.Store {
	if s.artifactDir == "" {
		return nil
	}
	maxBytes, err1 := parseSize(s.artifactMax)
	total, err2 := parseSize(s.artifactSize)
	if err := errors.Join(err1, err2); err != nil {
		log.Fatalf("manager: %v", err)
	}
	store, err := artifacts.Open(artifacts.Config{Dir: s.artifactDir, MaxBytes: maxBytes, TotalBytes: total})
	if err != nil {
		log.Printf("manager: standby: workload files won't be copied: %v", err)
		return nil
	}
	return store
}

// serveStandbyAPI serves the standby's operator API (status, promote) on
// -api-addr until stopped, retrying while the port is still held (by this
// process's own API a moment ago, after a step-down).
func (s standbySetup) serveStandbyAPI(r *failover.Replica) func() {
	srv := &http.Server{Handler: manager.StandbyRoleHandler(r, s.operatorToken), ReadHeaderTimeout: 10 * time.Second}
	done := make(chan struct{})
	go func() {
		for {
			ln, err := net.Listen("tcp", s.apiAddr)
			if err == nil {
				log.Printf("manager: standby: API on %s (harnessctl standby status | promote)", s.apiAddr)
				srv.Serve(ln)
				return
			}
			select {
			case <-done:
				return
			case <-time.After(time.Second):
			}
		}
	}()
	return func() {
		close(done)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}
}

// stepDown is the running manager's Config.StepDown: it writes the role
// file (standby of the newer manager) and stops the active manager; main
// then continues as a standby (afterRun).
type stepDown struct {
	db, fingerprint string
	cancel          context.CancelFunc
	happened        atomic.Bool
}

func (d *stepDown) wrap(ctx context.Context) context.Context {
	ctx, d.cancel = context.WithCancel(ctx)
	return ctx
}

func (d *stepDown) request(pair domain.StandbyPair, own, term uint64) {
	if _, err := failover.StepDown(d.db, d.fingerprint, pair, own, term); err != nil {
		log.Printf("manager: %v", err)
	}
	d.happened.Store(true)
	d.cancel()
}

// afterRun: when the manager stopped because it stepped down, it closes
// its database (the copy from the active manager replaces it) and goes on
// as a standby. Promoted again, it restarts itself (or exits for its
// launcher to restart it) as the active manager.
func (s standbySetup) afterRun(d *stepDown, close func() error) {
	if !d.happened.Load() {
		return
	}
	if err := close(); err != nil {
		log.Printf("manager: close database: %v", err)
	}
	if out, ok := s.run(); ok && out.Promoted {
		restartSelf()
	}
}

func restartSelf() {
	if os.Getenv("HOME_HARNESS_SUPERVISED") == "1" {
		log.Printf("manager: promoted: exiting for the launcher to start it again as the active manager")
		return
	}
	exe, err := os.Executable()
	if err == nil {
		cmd := exec.Command(exe, os.Args[1:]...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err = cmd.Start(); err == nil {
			log.Printf("manager: promoted: restarted as the active manager (pid %d)", cmd.Process.Pid)
			return
		}
	}
	log.Printf("manager: promoted, but couldn't restart (%v): start the manager again", err)
}
