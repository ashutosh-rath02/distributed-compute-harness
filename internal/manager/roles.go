package manager

import (
	"bytes"
	"io"
	"net/http"

	"home-harness/internal/domain"
)

// Who may call which operator-API route: ONE table, checked by
// requireOperator (operatorauth.go) after the credential is verified.
//
//	viewer   lists and gets: devices, work, events, files, the audit log.
//	         Never a secret, and never join or pairing information.
//	operator viewer + run and cancel work (workloads, jobs, workflows,
//	         plans and approving them), files in and out, split sessions,
//	         device availability, diagnostic commands. A person's daily use.
//	admin    everything else: policy, labels, revoking, the join window and
//	         approvals, invitations, updates, standby, remote access, users,
//	         rotating keys, and the keys and tokens themselves.
//
// A route that is not listed needs admin (fail closed): a route added later
// is admin-only until someone decides otherwise here.
// TestRouteRolesCoverEveryRoute fails if a route is registered in
// NewHTTPHandler but missing below.
var routeRoles = map[string]domain.Role{
	// viewer
	"GET /whoami":                  domain.RoleViewer,
	"GET /reliability":             domain.RoleViewer, // drain, disk and backup health
	"POST /backup":                 domain.RoleAdmin,  // holds the TLS key and every token
	"GET /metrics":                 domain.RoleViewer, // the metrics token has its own bypass (operatorauth.go)
	"GET /status":                  domain.RoleViewer,
	"GET /nodes":                   domain.RoleViewer,
	"GET /nodes/{id}":              domain.RoleViewer,
	"GET /nodes/{id}/resources":    domain.RoleViewer,
	"GET /nodes/{id}/capabilities": domain.RoleViewer,
	"GET /resources/total":         domain.RoleViewer,
	"GET /events":                  domain.RoleViewer,
	"GET /revocations":             domain.RoleViewer,
	"GET /agent-binaries":          domain.RoleViewer,
	"GET /agent-binary/hash":       domain.RoleViewer,
	"GET /join-window":             domain.RoleViewer, // open or closed, nothing more
	"GET /audit":                   domain.RoleViewer,
	"GET /workloads":               domain.RoleViewer,
	"GET /workloads/{id}":          domain.RoleViewer,
	"GET /artifacts":               domain.RoleViewer,
	"GET /artifacts/{sha}":         domain.RoleViewer,
	"GET /catalog":                 domain.RoleViewer,
	"GET /models":                  domain.RoleViewer,
	"GET /ai-devices":              domain.RoleViewer,
	"GET /llm/split":               domain.RoleViewer,
	"GET /policy":                  domain.RoleViewer,
	"GET /jobs":                    domain.RoleViewer,
	"GET /jobs/{id}":               domain.RoleViewer,
	"GET /workflows":               domain.RoleViewer,
	"GET /workflows/{id}":          domain.RoleViewer,
	"GET /plans":                   domain.RoleViewer,
	"GET /plans/{id}":              domain.RoleViewer,
	"GET /v1/models":               domain.RoleViewer,

	// operator
	"POST /nodes/{id}/commands":    domain.RoleOperator, // SELF_UPDATE and unknown commands: admin (commandRole)
	"POST /workloads":              domain.RoleOperator,
	"POST /workloads/{id}/cancel":  domain.RoleOperator,
	"POST /artifacts":              domain.RoleOperator,
	"DELETE /artifacts/{sha}":      domain.RoleOperator,
	"PUT /nodes/{id}/availability": domain.RoleOperator,
	"POST /llm/split":              domain.RoleOperator,
	"DELETE /llm/split":            domain.RoleOperator,
	"POST /jobs":                   domain.RoleOperator,
	"POST /jobs/{id}/cancel":       domain.RoleOperator,
	"POST /workflows":              domain.RoleOperator,
	"GET /schedules":               domain.RoleViewer,
	"GET /schedules/{id}":          domain.RoleViewer,
	"POST /schedules":              domain.RoleOperator,
	"POST /schedules/{id}/pause":   domain.RoleOperator,
	"POST /schedules/{id}/resume":  domain.RoleOperator,
	"POST /schedules/{id}/run":     domain.RoleOperator,
	"DELETE /schedules/{id}":       domain.RoleOperator,
	"POST /workflows/{id}/cancel":  domain.RoleOperator,
	"POST /plans":                  domain.RoleOperator,
	"POST /plans/{id}/approve":     domain.RoleOperator,
	"POST /plans/{id}/reject":      domain.RoleOperator,
	"POST /v1/chat/completions":    domain.RoleOperator, // spends the fleet's compute

	// admin: written out so the table reads as a whole (absent means the same)
	"POST /nodes/{id}/update":          domain.RoleAdmin,
	"POST /nodes/{id}/revoke":          domain.RoleAdmin,
	"DELETE /revocations/{id}":         domain.RoleAdmin,
	"PUT /nodes/{id}/meta":             domain.RoleAdmin,
	"DELETE /nodes/{id}/suspect":       domain.RoleAdmin,
	"PUT /policy":                      domain.RoleAdmin,
	"GET /join-info":                   domain.RoleAdmin, // pairing and relay tokens
	"GET /join-script":                 domain.RoleAdmin, // the pairing token
	"GET /join-requests":               domain.RoleAdmin, // pairing codes
	"POST /join-window":                domain.RoleAdmin,
	"DELETE /join-window":              domain.RoleAdmin,
	"POST /join-requests/{id}/approve": domain.RoleAdmin,
	"POST /join-requests/{id}/reject":  domain.RoleAdmin,
	"POST /enrollments":                domain.RoleAdmin,
	"GET /ai-info":                     domain.RoleAdmin, // the AI key
	"GET /standby":                     domain.RoleAdmin,
	"POST /standby":                    domain.RoleAdmin,
	"DELETE /standby":                  domain.RoleAdmin,
	"POST /standby/promote":            domain.RoleAdmin,
	"GET /remote-access":               domain.RoleAdmin,
	"PUT /remote-access":               domain.RoleAdmin,
	"POST /remote-access/rotate":       domain.RoleAdmin,
	"GET /users":                       domain.RoleAdmin,
	"POST /users":                      domain.RoleAdmin,
	"DELETE /users/{name}":             domain.RoleAdmin,
	"POST /users/{name}/token":         domain.RoleAdmin,
	"POST /users/{name}/rotate":        domain.RoleAdmin,
	"POST /rotate/{what}":              domain.RoleAdmin,
}

// publicRoutes are the patterns operatorPublic lets through without any
// credential; they are not in routeRoles (and the coverage test knows).
var publicRoutes = map[string]bool{
	"GET /{$}":                    true,
	"GET /live":                   true,
	"POST /login":                 true,
	"GET /server-proof":           true,
	"GET /enrollments/{token}/qr": true,
	// Probes and build identity (observe.go, version_api.go): fixed words
	// and version/commit/platform only, no data.
	"GET /healthz": true,
	"GET /readyz":  true,
	"GET /version": true,
}

// routeRole is the role a route pattern needs: unlisted means admin.
func routeRole(pattern string) domain.Role {
	if role, ok := routeRoles[pattern]; ok {
		return role
	}
	return domain.RoleAdmin
}

// roleAllows reports whether role may call pattern.
func roleAllows(role domain.Role, pattern string) bool {
	return role.Rank() >= routeRole(pattern).Rank()
}

// commandRole lifts a command dispatch above its route's role: only the
// diagnostics are operator-level (the same list remote access allows), an
// update or anything unknown is admin.
func commandRole(body []byte) domain.Role {
	if remoteCommandAllowed(body) {
		return domain.RoleOperator
	}
	return domain.RoleAdmin
}

// maxAPIBody caps request bodies on every route but file uploads (which
// stream under the artifact store's own limit): no JSON call needs more.
const maxAPIBody = 8 << 20

// requiredRole is the role r needs given the route the mux matched. For a
// command it reads the body (bounded) and puts it back for the handler.
func requiredRole(pattern string, r *http.Request) domain.Role {
	role := routeRole(pattern)
	if pattern == "POST /nodes/{id}/commands" && r.Body != nil {
		body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, maxAPIBody))
		if err != nil {
			return domain.RoleAdmin
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		if c := commandRole(body); c.Rank() > role.Rank() {
			role = c
		}
	}
	return role
}
