package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"home-harness/internal/agent"
	"home-harness/internal/domain"
	"home-harness/internal/manager"
	"home-harness/internal/mtls"
	relayproto "home-harness/internal/relay"
	relaytransport "home-harness/internal/transport/relay"
)

func TestInternetEnrollmentThroughPublicRelay(t *testing.T) {
	testInternetEnrollmentThroughPublicRelay(t, false)
}

func TestInternetEnrollmentThroughPublicRelayWithPinnedManagerTLS(t *testing.T) {
	testInternetEnrollmentThroughPublicRelay(t, true)
}

func testInternetEnrollmentThroughPublicRelay(t *testing.T, secure bool) {
	const privateSession = "private-manager-relay-session"
	rawListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	relayServer := relayproto.NewServerWithAliasKey(5*time.Second, "stable-alias-key")
	go relayServer.Serve(ctx, rawListener)
	public := httptest.NewServer(relayproto.NewPublicGateway(relayServer, rawListener.Addr().String(), "publish-secret").Handler())
	defer public.Close()

	binaryPath, _ := writeDummyAgentBinary(t, []byte("internet enrollment agent"))
	managerTransport := relaytransport.New(privateSession)
	fingerprint := ""
	if secure {
		cert, certErr := mtls.LoadOrCreateCert(t.TempDir())
		if certErr != nil {
			t.Fatal(certErr)
		}
		fingerprint = mtls.Fingerprint(cert)
		managerTransport = relaytransport.NewTLSServer(privateSession, cert)
	}
	srv := manager.NewServer(managerTransport, nil, manager.Config{
		Addr: rawListener.Addr().String(), PairingToken: pairingToken,
		HeartbeatTimeout: 2 * time.Second, AgentBinaries: dummyWindowsBuild(binaryPath),
		RelayAddr: rawListener.Addr().String(), RelayToken: privateSession,
		RelayPublicURL:      public.URL,
		Fingerprint:         fingerprint,
		EnrollmentPublisher: &manager.HTTPEnrollmentPublisher{URL: public.URL, RelaySession: privateSession, PublishToken: "publish-secret"},
	})
	managerTransport.Handle("/enroll/", srv.EnrollmentHandler())
	go srv.Run(ctx)
	operatorAPI := httptest.NewServer(srv.NewHTTPHandler())
	defer operatorAPI.Close()

	request := []byte(`{"mode":"remote","platform":"windows"}`)
	var invite struct {
		Token string `json:"token"`
		URL   string `json:"url"`
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, postErr := http.Post(operatorAPI.URL+"/enrollments", "application/json", bytes.NewReader(request))
		if postErr == nil && resp.StatusCode == http.StatusCreated {
			postErr = json.NewDecoder(resp.Body).Decode(&invite)
			resp.Body.Close()
			if postErr == nil {
				break
			}
		} else if resp != nil {
			resp.Body.Close()
		}
		if time.Now().After(deadline) {
			t.Fatalf("create remote invitation: %v", postErr)
		}
		time.Sleep(20 * time.Millisecond)
	}

	landing, err := http.Get(invite.URL)
	if err != nil {
		t.Fatalf("GET public landing page: %v", err)
	}
	landingBody, _ := io.ReadAll(landing.Body)
	landing.Body.Close()
	if landing.StatusCode != http.StatusOK || !strings.Contains(string(landingBody), "Join Home Compute Harness") {
		t.Fatalf("bad public landing page: status=%d body=%s", landing.StatusCode, landingBody)
	}
	setup, err := http.Get(invite.URL + "/setup")
	if err != nil {
		t.Fatalf("GET public setup: %v", err)
	}
	setupBody, _ := io.ReadAll(setup.Body)
	setup.Body.Close()
	if setup.StatusCode != http.StatusOK {
		t.Fatalf("bad setup status=%d body=%s", setup.StatusCode, setupBody)
	}
	binaryResp, err := http.Get(invite.URL + "/agent-binary")
	if err != nil {
		t.Fatalf("GET public agent binary: %v", err)
	}
	binaryBody, _ := io.ReadAll(binaryResp.Body)
	binaryResp.Body.Close()
	if binaryResp.StatusCode != http.StatusOK || string(binaryBody) != "internet enrollment agent" {
		t.Fatalf("bad public agent binary: status=%d body=%q", binaryResp.StatusCode, binaryBody)
	}
	setupText := string(setupBody)
	if strings.Contains(setupText, privateSession) {
		t.Fatal("public setup exposed the manager's private relay session")
	}
	match := regexp.MustCompile(`-relay-token ([^\s;]+)`).FindStringSubmatch(setupText)
	if len(match) != 2 || !strings.HasPrefix(match[1], "ha1.") {
		t.Fatalf("setup missing opaque per-device relay credential: %s", setupText)
	}

	agentCtx, stopAgent := context.WithCancel(context.Background())
	defer stopAgent()
	agentTransport := relaytransport.NewClient(match[1])
	if secure {
		agentTransport = relaytransport.NewTLSClient(match[1], mtls.PinnedClientConfig(fingerprint))
	}
	a, err := agent.New(agentTransport, agent.Config{DeviceUse: pluggedIn,
		ManagerAddr: rawListener.Addr().String(), PairingToken: invite.Token,
		IdentityDir: filepath.Join(t.TempDir(), "internet-enrolled-agent"), Name: "internet-enrolled-agent",
		HeartbeatInterval: 100 * time.Millisecond, ReconnectBackoff: 50 * time.Millisecond,
		Insecure: !secure, InsecureWorkloadsDisabled: !secure,
	})
	if err != nil {
		t.Fatal(err)
	}
	go a.Run(agentCtx)
	waitFor(t, 5*time.Second, func() bool {
		rec, ok := srv.Registry.Get(a.NodeID())
		return ok && rec.State == domain.NodeReady
	})
}
