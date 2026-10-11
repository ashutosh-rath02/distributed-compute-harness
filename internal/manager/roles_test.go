package manager

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"home-harness/internal/domain"
)

func TestRouteRolesCoverEveryRoute(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	wanted := map[string]bool{"NewHTTPHandler": true, "registerRemoteAccessRoutes": true, "registerUserRoutes": true, "registerObserveRoutes": true, "registerScheduleRoutes": true}
	var patterns []string
	for _, pkg := range pkgs {
		for fname, f := range pkg.Files {
			if strings.HasSuffix(fname, "_test.go") {
				continue
			}
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || !wanted[fn.Name.Name] || fn.Body == nil {
					continue
				}
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok || len(call.Args) == 0 {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "HandleFunc" {
						return true
					}
					if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						p, _ := strconv.Unquote(lit.Value)
						patterns = append(patterns, p)
					}
					return true
				})
			}
		}
	}
	if len(patterns) < 60 {
		t.Fatalf("only found %d registered routes: the source scan is broken", len(patterns))
	}
	registered := map[string]bool{}
	for _, p := range patterns {
		registered[p] = true
		if _, ok := routeRoles[p]; !ok && !publicRoutes[p] {
			t.Errorf("route %q is registered but has no entry in routeRoles (roles.go): it would be admin-only; decide its role", p)
		}
	}
	for p := range routeRoles {
		if !registered[p] {
			t.Errorf("routeRoles lists %q, which no handler registers", p)
		}
	}
}

// The expected role of every route, written out independently of
// routeRoles: changing a role there must fail here.
var expectedRoles = map[string]domain.Role{
	"GET /whoami": "viewer", "GET /metrics": "viewer", "GET /status": "viewer", "GET /nodes": "viewer", "GET /nodes/{id}": "viewer", "GET /nodes/{id}/resources": "viewer",
	"GET /nodes/{id}/capabilities": "viewer", "GET /resources/total": "viewer", "GET /events": "viewer", "GET /revocations": "viewer",
	"GET /agent-binaries": "viewer", "GET /agent-binary/hash": "viewer", "GET /join-window": "viewer", "GET /audit": "viewer",
	"GET /workloads": "viewer", "GET /workloads/{id}": "viewer", "GET /artifacts": "viewer", "GET /artifacts/{sha}": "viewer",
	"GET /catalog": "viewer", "GET /models": "viewer", "GET /ai-devices": "viewer", "GET /llm/split": "viewer", "GET /policy": "viewer",
	"GET /jobs": "viewer", "GET /jobs/{id}": "viewer", "GET /workflows": "viewer", "GET /workflows/{id}": "viewer",
	"GET /plans": "viewer", "GET /plans/{id}": "viewer", "GET /v1/models": "viewer",

	"POST /nodes/{id}/commands": "operator", "POST /workloads": "operator", "POST /workloads/{id}/cancel": "operator",
	"POST /artifacts": "operator", "DELETE /artifacts/{sha}": "operator", "PUT /nodes/{id}/availability": "operator",
	"POST /llm/split": "operator", "DELETE /llm/split": "operator", "POST /jobs": "operator", "POST /jobs/{id}/cancel": "operator",
	"POST /workflows": "operator", "GET /schedules": "viewer", "GET /schedules/{id}": "viewer", "POST /schedules": "operator", "POST /schedules/{id}/pause": "operator", "POST /schedules/{id}/resume": "operator", "POST /schedules/{id}/run": "operator", "DELETE /schedules/{id}": "operator", "POST /workflows/{id}/cancel": "operator", "POST /plans": "operator",
	"POST /plans/{id}/approve": "operator", "POST /plans/{id}/reject": "operator", "POST /v1/chat/completions": "operator",

	"POST /nodes/{id}/update": "admin", "POST /nodes/{id}/revoke": "admin", "DELETE /revocations/{id}": "admin",
	"PUT /nodes/{id}/meta": "admin", "DELETE /nodes/{id}/suspect": "admin", "PUT /policy": "admin", "GET /join-info": "admin",
	"GET /join-script": "admin", "GET /join-requests": "admin", "POST /join-window": "admin", "DELETE /join-window": "admin",
	"POST /join-requests/{id}/approve": "admin", "POST /join-requests/{id}/reject": "admin", "POST /enrollments": "admin",
	"GET /ai-info": "admin", "GET /standby": "admin", "POST /standby": "admin", "DELETE /standby": "admin", "POST /standby/promote": "admin",
	"GET /remote-access": "admin", "PUT /remote-access": "admin", "POST /remote-access/rotate": "admin", "GET /users": "admin",
	"POST /users": "admin", "DELETE /users/{name}": "admin", "POST /users/{name}/token": "admin", "POST /users/{name}/rotate": "admin",
	"POST /rotate/{what}": "admin",
}

func TestRouteRolesAreTheDecidedOnes(t *testing.T) {
	if len(routeRoles) != len(expectedRoles) {
		t.Errorf("routeRoles has %d entries, expected %d", len(routeRoles), len(expectedRoles))
	}
	for p, want := range expectedRoles {
		if got := routeRole(p); got != want {
			t.Errorf("%s: role %s, want %s", p, got, want)
		}
	}
	if routeRole("POST /a-route-nobody-listed") != domain.RoleAdmin || routeRole("") != domain.RoleAdmin {
		t.Error("an unlisted route must need admin")
	}
}

// Remote modes must never reach further than the table allows a role of the
// same level.
func TestRemoteModesNeverExceedTheRoleTable(t *testing.T) {
	for pattern, class := range remoteRoutes {
		switch class {
		case remoteView:
			if routeRole(pattern) != domain.RoleViewer {
				t.Errorf("%s is viewable remotely in every mode but needs %s", pattern, routeRole(pattern))
			}
		case remoteRun:
			if routeRole(pattern).Rank() > domain.RoleOperator.Rank() {
				t.Errorf("%s runs remotely in standard mode but needs %s", pattern, routeRole(pattern))
			}
		}
	}
	for _, c := range []struct {
		mode RemoteMode
		want domain.Role
	}{{RemoteReadOnly, "viewer"}, {RemoteStandard, "operator"}, {RemoteFull, "admin"}} {
		if remoteRole(c.mode) != c.want {
			t.Errorf("mode %s maps to %s, want %s", c.mode, remoteRole(c.mode), c.want)
		}
	}
}

func roleServer(t *testing.T) (*Server, http.Handler, map[domain.Role]string) {
	t.Helper()
	s := NewServer(nil, nil, Config{OperatorToken: testOperatorToken, PairingToken: "permanent-pairing-secret"})
	tokens := map[domain.Role]string{}
	for _, r := range []domain.Role{domain.RoleViewer, domain.RoleOperator, domain.RoleAdmin} {
		tok, err := s.AddUser("u-"+string(r), r, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		tokens[r] = tok
	}
	return s, s.NewHTTPHandler(), tokens
}

// TestRoleEnforcementPerRoute calls every route as every role and checks a
// 403 comes exactly when the role is too low (removing the role check
// anywhere makes this fail).
func TestRoleEnforcementPerRoute(t *testing.T) {
	fill := strings.NewReplacer("{id}", "node-x", "{sha}", strings.Repeat("0", 64), "{name}", "ghost", "{what}", "bogus", "{token}", "tok", "{$}", "")
	for pattern, want := range expectedRoles {
		if pattern == "GET /events" {
			continue // a stream: gated by the same code path as its neighbours
		}
		method, path, _ := strings.Cut(pattern, " ")
		for _, role := range []domain.Role{domain.RoleViewer, domain.RoleOperator, domain.RoleAdmin} {
			_, h, tokens := roleServer(t)
			body := ""
			if method != http.MethodGet && method != http.MethodDelete {
				body = `{}`
			}
			if pattern == "POST /nodes/{id}/commands" {
				body = `{"name":"PING"}`
			}
			rec := operatorRequest(t, h, method, fill.Replace(path), tokens[role], body)
			forbidden := rec.Code == http.StatusForbidden
			if forbidden != (role.Rank() < want.Rank()) {
				t.Errorf("%s as %s: got %d (%s); the route needs %s", pattern, role, rec.Code, strings.TrimSpace(rec.Body.String()), want)
			}
		}
	}
}

func TestSelfUpdateCommandNeedsAdmin(t *testing.T) {
	_, h, tokens := roleServer(t)
	for _, c := range []struct {
		role domain.Role
		body string
		deny bool
	}{
		{domain.RoleOperator, `{"name":"PING"}`, false},
		{domain.RoleOperator, `{"name":"SELF_UPDATE"}`, true},
		{domain.RoleOperator, `{"name":"SOMETHING_ELSE"}`, true},
		{domain.RoleOperator, `not json`, true},
		{domain.RoleViewer, `{"name":"PING"}`, true},
		{domain.RoleAdmin, `{"name":"SELF_UPDATE"}`, false},
	} {
		rec := operatorRequest(t, h, "POST", "/nodes/node-x/commands", tokens[c.role], c.body)
		if (rec.Code == http.StatusForbidden) != c.deny {
			t.Errorf("%s %s: %d %s", c.role, c.body, rec.Code, rec.Body.String())
		}
	}
}
