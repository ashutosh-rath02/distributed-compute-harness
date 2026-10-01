package manager

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"home-harness/internal/domain"
)

func TestRejectionAuditIsRateLimitedPerKey(t *testing.T) {
	s := NewServer(nil, nil, Config{})
	for i := 0; i < 20; i++ { // a revoked agent retrying, from a new ephemeral port each time
		s.auditRejection("node.rejected", "node-revoked", fmt.Sprintf("10.0.0.5:%d", 40000+i), revokedReason, nil)
	}
	s.auditRejection("node.rejected", "node-revoked", "10.0.0.5:4000", "invalid pairing token", nil) // different reason
	entries, _ := s.AuditEntries(domain.AuditNoise, 0)
	if len(entries) != 2 {
		t.Fatalf("expected one entry per (identity, reason) per window, got %d", len(entries))
	}
}

// A LAN device can mint a fresh identity per REGISTER, so per-key limits
// alone limit nothing: the global ceiling must hold, a summary must say
// how much was dropped, the limiter map must stay bounded, and the
// security log must be untouched.
func TestRejectionFloodIsCappedAndCannotTouchSecurityLog(t *testing.T) {
	s := NewServer(nil, nil, Config{})
	s.audit(domain.AuditSecurity, "node.revoked", "node-victim", actorOperatorToken, nil)
	for i := 0; i < 3000; i++ {
		s.auditRejection("node.rejected", domain.NodeID(fmt.Sprintf("node-fresh-%d", i)), "10.0.0.66:5000", "invalid pairing token", nil)
	}
	noise, _ := s.AuditEntries(domain.AuditNoise, 0)
	rejections := 0
	for _, e := range noise {
		if e.Kind == "node.rejected" {
			rejections++
		}
	}
	// The flood fits within one or two clock minutes in a test run.
	if rejections > 2*rejectionsPerMinute {
		t.Fatalf("global ceiling not enforced: %d rejection entries", rejections)
	}
	if keys := s.rejectionKeys(); keys > rejectionKeysMax {
		t.Fatalf("limiter map unbounded: %d keys", keys)
	}
	security, _ := s.AuditEntries(domain.AuditSecurity, 0)
	if len(security) != 1 || security[0].Kind != "node.revoked" {
		t.Fatalf("the flood must not touch the security log, got %+v", security)
	}

	// Once the minute rolls, the suppressed count is summarised.
	s.auditLog.mu.Lock()
	s.auditLog.minute = s.auditLog.minute.Add(-2 * rejectionWindow)
	s.auditLog.mu.Unlock()
	s.flushSuppressed() // the heartbeat ticker does this in a running manager
	noise, _ = s.AuditEntries(domain.AuditNoise, 1)
	if len(noise) != 1 || noise[0].Kind != "node.rejected-suppressed" || noise[0].Detail["suppressed"].(int) <= 0 {
		t.Fatalf("expected a suppression summary, got %+v", noise)
	}
}

func TestOperatorActionsRecordTheCredentialThatActed(t *testing.T) {
	s := NewServer(nil, nil, Config{OperatorToken: testOperatorToken})
	h := s.NewHTTPHandler()
	operatorRequest(t, h, http.MethodPost, "/login", "", `{"token":"`+testOperatorToken+`"}`)
	operatorRequest(t, h, http.MethodPost, "/login", "", `{"token":"wrong"}`)

	security, _ := s.AuditEntries(domain.AuditSecurity, 0)
	if len(security) != 1 || security[0].Kind != "operator.login" || security[0].Actor != actorDashboardSession {
		t.Fatalf("expected one successful login recorded for the dashboard session, got %+v", security)
	}
	noise, _ := s.AuditEntries(domain.AuditNoise, 0)
	if len(noise) != 1 || noise[0].Kind != "operator.login-failed" {
		t.Fatalf("expected the failed login in the noise log, got %+v", noise)
	}

	// The actor recorded on a mutation comes from which credential matched.
	s.Registry.Upsert(domain.Manifest{Node: domain.Node{Identity: domain.Identity{NodeID: "node-x"}}}, nil)
	operatorRequest(t, h, http.MethodPut, "/nodes/node-x/meta", testOperatorToken, `{"alias":"Desk"}`)
	operatorRequest(t, h, http.MethodPut, "/nodes/node-x/meta", dashboardSession(testOperatorToken), `{"alias":"Desk 2"}`)
	security, _ = s.AuditEntries(domain.AuditSecurity, 2)
	if len(security) != 2 || security[0].Actor != actorDashboardSession || security[1].Actor != actorOperatorToken {
		t.Fatalf("expected meta changes attributed to session then token, got %+v", security)
	}
}

// The security log is reserved for operator-credentialed actions: a
// peer-driven entry must never land there, whatever the caller asks.
func TestPeerDrivenEntriesNeverReachTheSecurityLog(t *testing.T) {
	s := NewServer(nil, nil, Config{})
	s.audit(domain.AuditSecurity, "node.admitted", "node-x", actorNode, nil)
	s.audit(domain.AuditSecurity, "something", "", actorAnonymous, nil)
	if security, _ := s.AuditEntries(domain.AuditSecurity, 0); len(security) != 0 {
		t.Fatalf("peer-driven entries reached the security log: %+v", security)
	}
	if admissions, _ := s.AuditEntries(domain.AuditAdmissions, 0); len(admissions) != 2 {
		t.Fatalf("expected the misrouted entries in admissions, got %+v", admissions)
	}
	s.audit(domain.AuditSecurity, "node.revoked", "node-x", actorOperatorToken, nil)
	merged, _ := s.AuditEntries("", 0)
	if len(merged) != 3 || merged[0].Kind != "node.revoked" {
		t.Fatalf("default view should merge security and admissions newest first, got %+v", merged)
	}
}

func TestClipBoundsPeerSuppliedStrings(t *testing.T) {
	long := strings.Repeat("é", 10000)
	if got := []rune(clip(long)); len(got) != 129 { // 128 + ellipsis
		t.Fatalf("clip left %d runes", len(got))
	}
	if clip("short") != "short" {
		t.Fatal("clip changed a short string")
	}
}
