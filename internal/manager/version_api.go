package manager

import (
	"net/http"

	"home-harness/internal/version"
)

// apiGetVersion reports which build this manager is: version, commit,
// platform. It is one of the few routes that needs no operator token (see
// operatorPublic), so a monitor, installer or support request can ask
// "what is running?" without a credential. It carries nothing about the
// fleet, the configuration or any secret, and never will: keep it that way.
func (s *Server) apiGetVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, version.Get())
}
