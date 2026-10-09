package manager

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"home-harness/internal/catalog"
	"home-harness/internal/domain"
)

// FuzzDraftPlan: a model's answer is attacker-influenced text. Parsing it
// must not panic or hang, and a plan it accepts may only name task types the
// planner offered.
func FuzzDraftPlan(f *testing.F) {
	f.Add(`{"possible":true,"summary":"s","task":{"type":"image.resize","params":{"width":100}},"files":["a.png"],"mode":"per-file"}`)
	f.Add("```json\n{\"possible\":false,\"reason\":\"no\"}\n```")
	f.Add(`{"possible":true,"task":{"type":"system.execute","params":{"cmd":"x"}}}`)
	f.Add(`{"task":{"type":"cpu.burn","params":{"seconds":[1,2]}},"then":[{"type":"archive.zip"}]}`)
	f.Add(`{{{{`)
	var offered []plannable
	for _, ty := range catalog.Types() {
		offered = append(offered, plannable{Type: ty, choices: map[string][]string{"model": {"gemma3:1b"}}})
	}
	files := []domain.ArtifactRef{{Name: "a.png", SHA256: strings.Repeat("a", 64)}, {Name: "b.png", SHA256: strings.Repeat("b", 64)}}
	f.Fuzz(func(t *testing.T, answer string) {
		d, err := draftPlan(answer, files, offered)
		if err != nil || !d.possible {
			return
		}
		ok := false
		for _, o := range offered {
			ok = ok || (d.task != nil && o.Name == d.task.Type)
		}
		if !ok {
			t.Fatalf("accepted a task type that was not offered: %+v", d.task)
		}
	})
}

// FuzzJoinBase: the Host header ends up inside shell and PowerShell
// scripts, so whatever joinBase accepts must be plainly host:port.
func FuzzJoinBase(f *testing.F) {
	for _, h := range []string{"192.168.1.5:7419", "[::1]:7419", "a'b:1", "x;rm -rf /:1", "$(id):1", "a b:1", "host:99999", "", "evil.com/x:1", "a\"b:80", "`x`:1"} {
		f.Add(h)
	}
	f.Fuzz(func(t *testing.T, host string) {
		r := httptest.NewRequest("GET", "/", nil)
		r.Host = host
		base, h := joinBase(r)
		if base == "" {
			return
		}
		if strings.ContainsAny(base+h, "'\"`$;&|<>()\\ \t\r\n{}*?!#~") {
			t.Fatalf("joinBase accepted a Host that is not plain host:port: %q", host)
		}
	})
}

// FuzzEnrollmentHandler: the agent-facing bootstrap paths with arbitrary
// tokens and sub-paths never panic and answer only 2xx/3xx/4xx.
func FuzzEnrollmentHandler(f *testing.F) {
	s := NewServer(nil, nil, Config{PairingToken: "p"})
	h := s.EnrollmentHandler()
	f.Add("/enroll/")
	f.Add("/enroll/abc/setup")
	f.Add("/enroll/abc/agent-binary/arm64")
	f.Add("/enroll//..//x")
	f.Fuzz(func(t *testing.T, path string) {
		if !strings.HasPrefix(path, "/") || strings.ContainsAny(path, " \r\n\x00?#") {
			return
		}
		r, err := http.NewRequest("GET", "http://127.0.0.1"+path, nil)
		if err != nil {
			return
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code >= 500 {
			t.Fatalf("%q -> %d", path, rec.Code)
		}
	})
}

// fuzzRoutes are the main body-taking routes.
var fuzzRoutes = []struct{ method, path string }{
	{"POST", "/workloads"}, {"POST", "/jobs"}, {"POST", "/workflows"}, {"POST", "/plans"},
	{"POST", "/nodes/node-x/commands"}, {"PUT", "/policy"}, {"PUT", "/nodes/node-x/meta"},
	{"PUT", "/nodes/node-x/availability"}, {"POST", "/enrollments"}, {"POST", "/join-window"},
	{"POST", "/llm/split"}, {"POST", "/users"}, {"POST", "/login"}, {"POST", "/standby"},
	{"PUT", "/remote-access"}, {"POST", "/v1/chat/completions"}, {"POST", "/artifacts"},
	{"POST", "/users/x/rotate"}, {"POST", "/plans/p1/approve"}, {"POST", "/nodes/node-x/revoke"},
}

// FuzzAPIBodies posts arbitrary bodies to the main routes of an in-memory
// manager (no listener). No panic, no 5xx, no hang. The server is rebuilt
// regularly so the harness's own state cannot grow without bound.
func FuzzAPIBodies(f *testing.F) {
	f.Add(uint8(0), []byte(`{"capability":"cpu.burn","params":{"seconds":"1"}}`))
	f.Add(uint8(1), []byte(`{"tasks":[{"capability":"cpu.burn","params":{"seconds":"1"}}],"maxAttempts":3}`))
	f.Add(uint8(2), []byte(`{"steps":[]}`))
	f.Add(uint8(3), []byte(`{"request":"resize my photos"}`))
	f.Add(uint8(4), []byte(`{"name":"PING","timeoutMs":-5}`))
	f.Add(uint8(5), []byte(`{"allowUnlisted":true,"types":{"cpu.burn":{"enabled":false}}}`))
	f.Add(uint8(11), []byte(`{"name":"a","role":"admin","expiresDays":99999999}`))
	f.Add(uint8(15), []byte(`{"model":"x","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	var s *Server
	var h http.Handler
	n := 0
	f.Fuzz(func(t *testing.T, which uint8, body []byte) {
		if s == nil || n%300 == 0 {
			s = NewServer(nil, nil, Config{PairingToken: "p"})
			h = s.NewHTTPHandler()
		}
		n++
		route := fuzzRoutes[int(which)%len(fuzzRoutes)]
		req := httptest.NewRequest(route.method, route.path, strings.NewReader(string(body)))
		req.Host = "127.0.0.1:7421"
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		done := make(chan struct{})
		go func() {
			defer close(done)
			h.ServeHTTP(rec, req)
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatalf("%s %s hung on body %q", route.method, route.path, body)
		}
		if rec.Code >= 500 && rec.Code != http.StatusBadGateway && rec.Code != http.StatusGatewayTimeout {
			io.Copy(io.Discard, rec.Body)
			t.Fatalf("%s %s -> %d for body %q: %s", route.method, route.path, rec.Code, body, rec.Body.String())
		}
	})
}
