package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"home-harness/internal/domain"
	"home-harness/internal/identity"
	"home-harness/internal/manager"
	"home-harness/internal/protocol"
	"home-harness/internal/transport/ws"
)

// registerRawNodeWithBinaryHash is registerRawSilentNode
// (command_disconnect_test.go) with a configurable BinaryHash in the
// manifest — needed here to control exactly whether NeedsUpdate should
// consider this node current or stale, without depending on any real
// internal/agent code (which would try to replace its own binary; see
// TestSelfUpdateCommandDispatchedWithCorrectHash's doc comment).
func registerRawNodeWithBinaryHash(t *testing.T, addr, name, binaryHash string) (domain.NodeID, domain.Conn) {
	t.Helper()
	id, err := identity.LoadOrCreate(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatalf("identity.LoadOrCreate: %v", err)
	}

	conn := dialWithRetry(t, addr)

	manifest := domain.Manifest{
		SchemaVersion: domain.ManifestSchemaVersion,
		Node: domain.Node{Identity: id.Identity, Name: name, BinaryHash: binaryHash,
			Platform: domain.Platform{OS: "windows", Architecture: "amd64"}},
		Capabilities: []domain.Capability{{Name: domain.CapabilitySystemExecute}},
		// Behaves like a current agent build, which follows the
		// SELF_UPDATE command's per-platform path.
		AgentFeatures: []string{domain.FeatureSelfUpdatePath},
	}
	signature := id.Sign(protocol.RegisterSignedData(pairingToken, id.NodeID))
	env, err := protocol.NewEnvelope(protocol.MsgRegister, id.NodeID, domain.ManagerNodeID,
		protocol.RegisterPayload{Manifest: manifest, PairingToken: pairingToken, Signature: signature})
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	wire, err := protocol.Encode(env)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := conn.Send(ctx, wire); err != nil {
		t.Fatalf("send REGISTER: %v", err)
	}

	data, err := conn.Receive(ctx)
	if err != nil {
		t.Fatalf("receive REGISTER_ACK: %v", err)
	}
	ackEnv, err := protocol.Decode(data)
	if err != nil {
		t.Fatalf("decode REGISTER_ACK: %v", err)
	}
	if ackEnv.Type != protocol.MsgRegisterAck {
		t.Fatalf("expected REGISTER_ACK, got %s", ackEnv.Type)
	}

	return id.NodeID, conn
}

// startManagerWithAgentBinary starts a manager with AgentBinaryPath set,
// wiring /agent-binary onto the same transport agents connect over — the
// exact wiring cmd/manager/main.go does, so this proves the real mechanism,
// not a test-only shortcut.
func startManagerWithAgentBinary(t *testing.T, addr, binaryPath string) *manager.Server {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	transport := ws.New()
	srv := manager.NewServer(transport, nil, manager.Config{
		Addr:             addr,
		PairingToken:     pairingToken,
		HeartbeatTimeout: 2 * time.Second,
		AgentBinaries:    dummyWindowsBuild(binaryPath),
	})
	transport.Handle("/agent-binary", srv.AgentBinaryHandler())
	transport.Handle("GET /agent-binaries/{os}/{arch}", srv.AgentBinariesHandler())
	go func() {
		if err := srv.Run(ctx); err != nil && err != context.Canceled {
			t.Logf("manager exited: %v", err)
		}
	}()
	waitListening(t, addr)
	return srv
}

// dummyWindowsBuild declares a dummy-content test binary as the catalog's
// windows/amd64 build. Its content has no executable header to detect,
// so the platform is stated explicitly — as an operator would with
// -agent-binary os/arch=path.
func dummyWindowsBuild(path string) []manager.AgentBinary {
	return []manager.AgentBinary{{OS: "windows", Arch: "amd64", Path: path}}
}

func writeDummyAgentBinary(t *testing.T, content []byte) (path, hash string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "agent.exe")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	sum := sha256.Sum256(content)
	return path, hex.EncodeToString(sum[:])
}

// TestAgentBinaryEndpointServesConfiguredFile proves the actual download
// mechanism a real agent's self-update relies on: the manager's own cached
// hash matches what /agent-binary serves, fetched over the exact
// address/port agents already dial for their WebSocket connection (no
// separate port).
func TestAgentBinaryEndpointServesConfiguredFile(t *testing.T) {
	const addr = "127.0.0.1:19295"
	content := []byte("pretend-agent-binary-content-v2")
	binaryPath, wantHash := writeDummyAgentBinary(t, content)

	srv := startManagerWithAgentBinary(t, addr, binaryPath)
	if srv.AgentBinaryHash() != wantHash {
		t.Fatalf("expected cached hash %q, got %q", wantHash, srv.AgentBinaryHash())
	}

	var resp *http.Response
	var err error
	deadline := time.Now().Add(2 * time.Second)
	for {
		resp, err = http.Get("http://" + addr + "/agent-binary")
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET /agent-binary: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(body, content) {
		t.Fatalf("expected served content to match the configured file, got %q", body)
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != wantHash {
		t.Fatal("expected the downloaded bytes' hash to match the manager's cached hash")
	}
}

// TestUpdateEndpointAlreadyUpToDate proves POST /nodes/{id}/update
// short-circuits (no COMMAND ever sent) for a node already matching the
// served binary — exercised through the real HTTP API, not just the
// underlying NeedsUpdate check.
func TestUpdateEndpointAlreadyUpToDate(t *testing.T) {
	const addr = "127.0.0.1:19296"
	binaryPath, wantHash := writeDummyAgentBinary(t, []byte("content"))
	srv := startManagerWithAgentBinary(t, addr, binaryPath)
	apiSrv := httptest.NewServer(srv.NewHTTPHandler())
	defer apiSrv.Close()

	nodeID, conn := registerRawNodeWithBinaryHash(t, addr, "update-agent-current", wantHash)
	defer conn.Close()

	waitFor(t, 3*time.Second, func() bool {
		rec, ok := srv.Registry.Get(nodeID)
		return ok && rec.State == domain.NodeReady
	})

	resp, err := http.Post(fmt.Sprintf("%s/nodes/%s/update", apiSrv.URL, nodeID), "application/json", nil)
	if err != nil {
		t.Fatalf("POST /nodes/{id}/update: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	var result map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result["status"] != "already-up-to-date" {
		t.Fatalf("expected already-up-to-date, got %+v", result)
	}
}

// TestSelfUpdateCommandDispatchedWithCorrectHash proves the manager's
// trigger path end to end over the real wire protocol: a node whose
// reported BinaryHash differs from the served binary receives a real
// SELF_UPDATE COMMAND naming the exact expected sha256, and the manager's
// SendCommand correctly resolves once that (raw, test-controlled) node
// acks it — exactly the exchange a real agent's handleCommand special-case
// produces (see internal/agent/commands.go), without running any real
// agent code (which would attempt to replace its own binary — internal/
// agent's own tests cover that mechanism safely and directly).
func TestSelfUpdateCommandDispatchedWithCorrectHash(t *testing.T) {
	const addr = "127.0.0.1:19297"
	binaryPath, wantHash := writeDummyAgentBinary(t, []byte("new content"))
	srv := startManagerWithAgentBinary(t, addr, binaryPath)
	apiSrv := httptest.NewServer(srv.NewHTTPHandler())
	defer apiSrv.Close()

	nodeID, conn := registerRawNodeWithBinaryHash(t, addr, "update-agent-stale", "stale-hash")
	defer conn.Close()

	waitFor(t, 3*time.Second, func() bool {
		rec, ok := srv.Registry.Get(nodeID)
		return ok && rec.State == domain.NodeReady
	})

	rec, _ := srv.Registry.Get(nodeID)
	if status := srv.UpdateStatusFor(rec); status != manager.UpdateAvailable {
		t.Fatalf("expected an update available for a node with a stale BinaryHash, got %s", status)
	}

	// Reply to the incoming SELF_UPDATE COMMAND exactly as a real agent's
	// handleCommand special-case would, on our own goroutine so
	// SendCommand (blocking, below) has something to receive.
	replyDone := make(chan error, 1)
	go func() {
		recvCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		data, err := conn.Receive(recvCtx)
		if err != nil {
			replyDone <- err
			return
		}
		env, err := protocol.Decode(data)
		if err != nil {
			replyDone <- err
			return
		}
		var payload protocol.CommandPayload
		if err := env.DecodePayload(&payload); err != nil {
			replyDone <- err
			return
		}
		if payload.Command.Name != domain.CommandSelfUpdate {
			replyDone <- io.EOF // wrong command name, fail the test below
			return
		}
		// The path names the node's own platform's catalog route.
		if payload.Command.Args["sha256"] != wantHash || payload.Command.Args["path"] != "/agent-binaries/windows/amd64" {
			replyDone <- io.EOF
			return
		}
		result := domain.CommandResult{CommandID: payload.Command.ID, Success: true, Output: map[string]string{"status": "update started"}}
		ackEnv, err := protocol.NewEnvelope(protocol.MsgCommandResult, nodeID, domain.ManagerNodeID, protocol.CommandResultPayload{Result: result})
		if err != nil {
			replyDone <- err
			return
		}
		wire, err := protocol.Encode(ackEnv)
		if err != nil {
			replyDone <- err
			return
		}
		replyDone <- conn.Send(recvCtx, wire)
	}()

	resp, err := http.Post(fmt.Sprintf("%s/nodes/%s/update", apiSrv.URL, nodeID), "application/json", nil)
	if err != nil {
		t.Fatalf("POST /nodes/{id}/update: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	var result domain.CommandResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected a successful ack, got %+v", result)
	}

	if err := <-replyDone; err != nil {
		t.Fatalf("raw node's reply goroutine reported an error (wrong command/hash?): %v", err)
	}
}
