package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"home-harness/internal/agent"
	"home-harness/internal/domain"
	"home-harness/internal/manager"
	"home-harness/internal/mtls"
	"home-harness/internal/transport/ws"
	"home-harness/internal/tunnel"
)

// echoService is a TCP echo server on loopback standing in for a helper's
// local service; kill drops every connection it has.
type echoService struct {
	ln    net.Listener
	mu    sync.Mutex
	conns []net.Conn
}

func startEcho(t *testing.T) *echoService {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	e := &echoService{ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			e.mu.Lock()
			e.conns = append(e.conns, c)
			e.mu.Unlock()
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	return e
}

func (e *echoService) port() int { return e.ln.Addr().(*net.TCPAddr).Port }

func (e *echoService) kill() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, c := range e.conns {
		c.Close()
	}
}

func startTunnelManager(t *testing.T, addr string) artifactManager {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	transport := ws.New()
	srv := manager.NewServer(transport, nil, manager.Config{Addr: addr, PairingToken: pairingToken, HeartbeatTimeout: 5 * time.Second, ReconcileInterval: 100 * time.Millisecond})
	transport.Handle("/tunnel/", srv.TunnelHandler())
	go srv.Run(ctx)
	waitListening(t, addr)
	api := httptest.NewServer(srv.NewHTTPHandler())
	t.Cleanup(api.Close)
	return artifactManager{srv: srv, api: api.URL}
}

func startTunnelAgent(t *testing.T, ctx context.Context, m artifactManager, addr, name string) *agent.Agent {
	t.Helper()
	a, err := agent.New(ws.New(), agent.Config{DeviceUse: pluggedIn,
		ManagerAddr: addr, PairingToken: pairingToken, IdentityDir: filepath.Join(t.TempDir(), name), WorkDir: filepath.Join(t.TempDir(), name+"-work"),
		Name: name, HeartbeatInterval: 100 * time.Millisecond, HostFingerprint: "-", Insecure: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	go a.Run(ctx)
	id := a.NodeID()
	waitFor(t, 5*time.Second, func() bool { rec, ok := m.srv.Registry.Get(id); return ok && rec.State == domain.NodeReady })
	return a
}

// echoRoundTrip sends n random bytes through conn and checks the echo.
func echoRoundTrip(t *testing.T, conn net.Conn, n int) error {
	data := make([]byte, n)
	rand.Read(data)
	sum := sha256.Sum256(data)
	errc := make(chan error, 1)
	go func() {
		_, err := io.Copy(conn, bytes.NewReader(data))
		errc <- err
	}()
	h := sha256.New()
	if _, err := io.CopyN(h, conn, int64(n)); err != nil {
		return err
	}
	if err := <-errc; err != nil {
		return err
	}
	if !bytes.Equal(h.Sum(nil), sum[:]) {
		return io.ErrUnexpectedEOF
	}
	return nil
}

// Over TLS, as real devices connect: each agent's side of the tunnel
// goes through the manager certificate it pins.
func TestTunnelOverPinnedTLS(t *testing.T) {
	const addr = "127.0.0.1:19601"
	cert, err := mtls.LoadOrCreateCert(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	transport := ws.NewTLSServer(cert)
	srv := manager.NewServer(transport, nil, manager.Config{Addr: addr, PairingToken: pairingToken, HeartbeatTimeout: 5 * time.Second})
	transport.Handle("/tunnel/", srv.TunnelHandler())
	go srv.Run(ctx)
	waitListening(t, addr)
	start := func(name string) *agent.Agent {
		a, err := agent.New(ws.NewTLSClient(mtls.PinnedClientConfig(mtls.Fingerprint(cert))), agent.Config{DeviceUse: pluggedIn,
			ManagerAddr: addr, PairingToken: pairingToken, IdentityDir: filepath.Join(t.TempDir(), name), WorkDir: filepath.Join(t.TempDir(), name+"-work"),
			Name: name, HeartbeatInterval: 100 * time.Millisecond, HostFingerprint: "-", ManagerFingerprint: mtls.Fingerprint(cert),
		})
		if err != nil {
			t.Fatal(err)
		}
		go a.Run(ctx)
		id := a.NodeID()
		waitFor(t, 5*time.Second, func() bool { rec, ok := srv.Registry.Get(id); return ok && rec.State == domain.NodeReady })
		return a
	}
	main, helper := start("tls-main"), start("tls-helper")
	echo := startEcho(t)
	mainToken, helperToken, err := srv.OpenTunnelPair("s2", 0, main.NodeID(), helper.NodeID())
	if err != nil {
		t.Fatal(err)
	}
	helper.ExposeService("s2", 0, echo.port(), helperToken)
	local, err := main.TunnelListen(ctx, mainToken)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", local)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := echoRoundTrip(t, conn, 20<<20); err != nil {
		t.Fatalf("20 MB over TLS: %v", err)
	}
}

func TestTunnelCarriesAServiceBetweenDevices(t *testing.T) {
	const addr = "127.0.0.1:19600"
	m := startTunnelManager(t, addr)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	main := startTunnelAgent(t, ctx, m, addr, "main")
	helper := startTunnelAgent(t, ctx, m, addr, "helper")
	echo := startEcho(t)

	mainToken, helperToken, err := m.srv.OpenTunnelPair("s1", 0, main.NodeID(), helper.NodeID())
	if err != nil {
		t.Fatal(err)
	}
	helper.ExposeService("s1", 0, echo.port(), helperToken)
	local, err := main.TunnelListen(ctx, mainToken)
	if err != nil {
		t.Fatal(err)
	}

	// 200 MB through, byte for byte.
	conn, err := net.Dial("tcp", local)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := echoRoundTrip(t, conn, 200<<20); err != nil {
		t.Fatalf("200 MB echo: %v", err)
	}
	conn.Close()
	t.Logf("200 MB each way in %v", time.Since(start).Round(time.Millisecond))

	// Several at once.
	var wg sync.WaitGroup
	errs := make(chan error, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := net.Dial("tcp", local)
			if err != nil {
				errs <- err
				return
			}
			defer c.Close()
			errs <- echoRoundTrip(t, c, 5<<20)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent connection: %v", err)
		}
	}

	// The helper's service dies mid-transfer: the main side sees the end.
	conn, _ = net.Dial("tcp", local)
	conn.Write([]byte("hello"))
	buf := make([]byte, 5)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("before the drop: %v", err)
	}
	echo.kill()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Read(buf); err == nil {
		t.Fatal("still open after the helper's service went away")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("the main side wasn't told the helper's service went away")
	}
	conn.Close()

	// A token nobody issued, and the session's own once it has ended.
	if _, err := tunnel.Dial(ctx, "ws://"+addr+"/tunnel/0123456789abcdef", http.DefaultClient); err == nil {
		t.Fatal("a made-up token was accepted")
	}
	m.srv.CloseTunnelSession("s1")
	if _, err := tunnel.Dial(ctx, "ws://"+addr+"/tunnel/"+mainToken, http.DefaultClient); err == nil {
		t.Fatal("a token of an ended session was accepted")
	}
	conn, _ = net.Dial("tcp", local) // the main device's port now leads nowhere
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Read(buf); err == nil {
		t.Fatal("a connection after the session ended got data")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("a connection after the session ended was left hanging")
	}
	// The helper-side token can't be used as a main side either.
	if _, err := tunnel.Dial(ctx, "ws://"+addr+"/tunnel/"+helperToken+"?conn=x", http.DefaultClient); err == nil {
		t.Fatal("an ended helper token was accepted")
	}
}
