package manager

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// docs/openapi.yaml describes the operator API. These tests keep it honest:
// every route the handler registers must be in the spec, every operation in
// the spec must be a real route, the spec must be well formed, and what it
// says about authentication must match what the handler does.
//
// Routes are read from the source (go/ast), starting at NewHTTPHandler and
// following every helper it passes the mux to (registerRemoteAccessRoutes
// and whatever is added later). The agent-facing muxes (workload file
// transfer, the join page, the standby handler, the relay) are other
// functions and are deliberately not part of this API.

// undocumentedRoutes lists routes that exist but are not in the spec yet.
// It exists only so that parallel work does not break the build: a route
// added by someone else lands here ("METHOD /path" -> who added it), and the
// merge owner documents it in docs/openapi.yaml and deletes the entry. An
// entry for a route that is now documented, or no longer exists, fails the
// test, so this list can only shrink.
var undocumentedRoutes = map[string]string{}

const openapiPath = "../../docs/openapi.yaml"

// ---- routes in the code

func codeRoutes(t *testing.T) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	funcs := map[string]*ast.FuncDecl{} // by name; methods and functions share the namespace
	for _, e := range entries {
		n := e.Name()
		if !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, n, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", n, err)
		}
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil {
				funcs[fd.Name.Name] = fd
			}
		}
	}
	root := funcs["NewHTTPHandler"]
	if root == nil {
		t.Fatal("NewHTTPHandler not found")
	}
	routes := map[string]bool{}
	seen := map[string]bool{}
	var walk func(fd *ast.FuncDecl)
	walk = func(fd *ast.FuncDecl) {
		if seen[fd.Name.Name] {
			return
		}
		seen[fd.Name.Name] = true
		muxes := map[string]bool{}
		if fd.Type.Params != nil {
			for _, p := range fd.Type.Params.List {
				if star, ok := p.Type.(*ast.StarExpr); ok {
					if sel, ok := star.X.(*ast.SelectorExpr); ok && sel.Sel.Name == "ServeMux" {
						for _, n := range p.Names {
							muxes[n.Name] = true
						}
					}
				}
			}
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if as, ok := n.(*ast.AssignStmt); ok && len(as.Lhs) == 1 && len(as.Rhs) == 1 {
				if call, ok := as.Rhs[0].(*ast.CallExpr); ok {
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "NewServeMux" {
						if id, ok := as.Lhs[0].(*ast.Ident); ok {
							muxes[id.Name] = true
						}
					}
				}
			}
			return true
		})
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			// mux.HandleFunc("METHOD /path", ...) / mux.Handle(...)
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok && muxes[id.Name] && (sel.Sel.Name == "HandleFunc" || sel.Sel.Name == "Handle") {
					if len(call.Args) == 0 {
						t.Fatalf("%s: %s.%s with no pattern", fset.Position(call.Pos()), id.Name, sel.Sel.Name)
					}
					lit, ok := call.Args[0].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						t.Fatalf("%s: route patterns must be string literals so docs/openapi.yaml can be checked against them", fset.Position(call.Pos()))
					}
					pat, _ := strconv.Unquote(lit.Value)
					key, err := normalizePattern(pat)
					if err != nil {
						t.Fatalf("%s: %v", fset.Position(call.Pos()), err)
					}
					routes[key] = true
					return true
				}
			}
			// helper(mux) / s.helper(mux): follow it.
			passes := false
			for _, a := range call.Args {
				if id, ok := a.(*ast.Ident); ok && muxes[id.Name] {
					passes = true
				}
			}
			if !passes {
				return true
			}
			var name string
			switch f := call.Fun.(type) {
			case *ast.Ident:
				name = f.Name
			case *ast.SelectorExpr:
				name = f.Sel.Name
			}
			target := funcs[name]
			if target == nil {
				t.Fatalf("%s: the mux is passed to %s, which this test cannot find in package manager", fset.Position(call.Pos()), name)
			}
			walk(target)
			return true
		})
	}
	walk(root)
	if len(routes) < 50 {
		t.Fatalf("found only %d routes in the code: the route scan is broken", len(routes))
	}
	return routes
}

var wildcardRest = regexp.MustCompile(`\{([A-Za-z0-9_]+)\.\.\.\}`)

// normalizePattern turns a ServeMux pattern into "METHOD /path/{param}".
func normalizePattern(pat string) (string, error) {
	method, path, ok := strings.Cut(pat, " ")
	if !ok || !strings.HasPrefix(path, "/") || strings.ToUpper(method) != method {
		return "", fmt.Errorf("route %q must be written as \"METHOD /path\" so it can be documented", pat)
	}
	path = strings.ReplaceAll(path, "{$}", "")
	if path == "" {
		path = "/"
	}
	path = wildcardRest.ReplaceAllString(path, "{$1}")
	return method + " " + path, nil
}

// ---- routes in the spec

var httpMethods = []string{"get", "put", "post", "delete", "patch", "head", "options"}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func specDoc(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(openapiPath))
	if err != nil {
		t.Fatalf("read the OpenAPI spec: %v", err)
	}
	v, err := parseYAMLSubset(string(raw))
	if err != nil {
		t.Fatalf("docs/openapi.yaml: %v", err)
	}
	doc := asMap(v)
	if doc == nil {
		t.Fatal("docs/openapi.yaml: top level must be a mapping")
	}
	return doc
}

func specRoutes(doc map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for path, item := range asMap(doc["paths"]) {
		for _, m := range httpMethods {
			if op := asMap(asMap(item)[m]); op != nil {
				out[strings.ToUpper(m)+" "+path] = op
			}
		}
	}
	return out
}

func sortedRouteKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestOpenAPIMatchesRegisteredRoutes(t *testing.T) {
	code := codeRoutes(t)
	spec := specRoutes(specDoc(t))

	var missing, phantom, stale []string
	for _, r := range sortedRouteKeys(code) {
		_, documented := spec[r]
		_, waived := undocumentedRoutes[r]
		switch {
		case !documented && !waived:
			missing = append(missing, r)
		case documented && waived:
			stale = append(stale, r+" (documented now: delete it from undocumentedRoutes)")
		}
	}
	for _, r := range sortedRouteKeys(spec) {
		if !code[r] {
			phantom = append(phantom, r)
		}
	}
	for _, r := range sortedRouteKeys(undocumentedRoutes) {
		if !code[r] {
			stale = append(stale, r+" (no such route any more: delete it from undocumentedRoutes)")
		}
	}
	if len(missing) > 0 {
		t.Errorf("these routes are registered but not documented. Add each to docs/openapi.yaml (path, method, operationId, request and response schemas, auth) as part of the change that adds the route:\n  %s\n(Only if the documentation must wait, add it to undocumentedRoutes in %s with a reason; that list is meant to be empty.)",
			strings.Join(missing, "\n  "), "internal/manager/openapi_test.go")
	}
	if len(phantom) > 0 {
		t.Errorf("docs/openapi.yaml documents operations that no handler registers (renamed or removed? fix the spec):\n  %s", strings.Join(phantom, "\n  "))
	}
	if len(stale) > 0 {
		t.Errorf("undocumentedRoutes has stale entries:\n  %s", strings.Join(stale, "\n  "))
	}
}

// resolveRef follows "#/components/..." references.
func resolveRef(doc map[string]any, ref string) any {
	if !strings.HasPrefix(ref, "#/") {
		return nil
	}
	var cur any = doc
	for _, part := range strings.Split(ref[2:], "/") {
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		m := asMap(cur)
		if m == nil {
			return nil
		}
		cur = m[part]
	}
	return cur
}

// walkRefs calls fn for every $ref string under v.
func walkRefs(v any, fn func(ref string)) {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if k == "$ref" {
				if s, ok := val.(string); ok {
					fn(s)
				}
				continue
			}
			walkRefs(val, fn)
		}
	case []any:
		for _, e := range x {
			walkRefs(e, fn)
		}
	}
}

var pathParam = regexp.MustCompile(`\{([^}]+)\}`)

func TestOpenAPIIsWellFormed(t *testing.T) {
	doc := specDoc(t)
	if v, _ := doc["openapi"].(string); !strings.HasPrefix(v, "3.1.") {
		t.Errorf("openapi must be a 3.1.x version string, got %v", doc["openapi"])
	}
	info := asMap(doc["info"])
	for _, k := range []string{"title", "version", "description"} {
		if s, _ := info[k].(string); strings.TrimSpace(s) == "" {
			t.Errorf("info.%s is required", k)
		}
	}

	// Every $ref resolves.
	walkRefs(doc, func(ref string) {
		if resolveRef(doc, ref) == nil {
			t.Errorf("$ref %s does not resolve", ref)
		}
	})

	schemes := asMap(asMap(doc["components"])["securitySchemes"])
	for _, name := range []string{"bearerAuth", "aiKey"} {
		if asMap(schemes[name]) == nil {
			t.Errorf("components.securitySchemes.%s is missing", name)
		}
	}

	ids := map[string]string{}
	for _, key := range sortedRouteKeys(specRoutes(doc)) {
		op := specRoutes(doc)[key]
		method, path, _ := strings.Cut(key, " ")

		id, _ := op["operationId"].(string)
		if id == "" {
			t.Errorf("%s: operationId is required", key)
		} else if prev, dup := ids[id]; dup {
			t.Errorf("%s: operationId %q is also used by %s", key, id, prev)
		} else {
			ids[id] = key
		}
		if s, _ := op["summary"].(string); strings.TrimSpace(s) == "" {
			t.Errorf("%s: summary is required", key)
		}
		if tags, _ := op["tags"].([]any); len(tags) == 0 {
			t.Errorf("%s: tags are required", key)
		}
		responses := asMap(op["responses"])
		if len(responses) == 0 {
			t.Errorf("%s: responses are required", key)
		}
		success := false
		for code := range responses {
			if strings.HasPrefix(code, "2") {
				success = true
			}
			if len(code) != 3 {
				t.Errorf("%s: response code %q must be three digits (quoted)", key, code)
			}
		}
		if !success {
			t.Errorf("%s: no 2xx response documented", key)
		}
		// A body-carrying method that takes a body documents it; a GET must not.
		if method == "GET" && op["requestBody"] != nil {
			t.Errorf("%s: GET with a requestBody", key)
		}

		// Every {param} in the path is declared as a required path parameter,
		// and every path parameter declared is in the path.
		declared := map[string]bool{}
		var params []any
		params = append(params, asMap(asMap(doc["paths"])[path])["parameters"])
		if ps, ok := op["parameters"].([]any); ok {
			params = append(params, ps...)
		}
		for _, p := range params {
			pm := asMap(p)
			if pm == nil {
				continue
			}
			if ref, ok := pm["$ref"].(string); ok {
				pm = asMap(resolveRef(doc, ref))
			}
			if pm["in"] == "path" {
				if pm["required"] != true {
					t.Errorf("%s: path parameter %v must be required: true", key, pm["name"])
				}
				declared[fmt.Sprint(pm["name"])] = true
			}
		}
		inPath := map[string]bool{}
		for _, m := range pathParam.FindAllStringSubmatch(path, -1) {
			inPath[m[1]] = true
			if !declared[m[1]] {
				t.Errorf("%s: path parameter {%s} is not declared", key, m[1])
			}
		}
		for name := range declared {
			if !inPath[name] {
				t.Errorf("%s: declares path parameter %q that is not in the path", key, name)
			}
		}

		// Authentication: /v1 takes the AI key as well as the operator token;
		// nothing else mentions the AI key.
		sec, hasSec := op["security"].([]any)
		mentions := func(scheme string) bool {
			for _, req := range sec {
				if _, ok := asMap(req)[scheme]; ok {
					return true
				}
			}
			return false
		}
		if strings.HasPrefix(path, "/v1/") {
			if !hasSec || !mentions("aiKey") || !mentions("bearerAuth") {
				t.Errorf("%s: /v1 routes must list both aiKey and bearerAuth", key)
			}
		} else if mentions("aiKey") {
			t.Errorf("%s: only /v1 routes accept the AI key", key)
		}
	}
}

// publicRoutes are the routes the handler answers without a credential
// (operatorPublic in operatorauth.go). The spec marks exactly these with
// `security: []`.
var publicRoutes = map[string]bool{
	"GET /": true, "GET /live": true, "POST /login": true,
	"GET /server-proof": true, "GET /enrollments/{token}/qr": true,
}

func TestOpenAPISecurityMatchesHandler(t *testing.T) {
	spec := specRoutes(specDoc(t))
	s := NewServer(nil, nil, Config{OperatorToken: testOperatorToken, AIKey: "ai-key-for-the-openapi-test"})
	h := s.NewHTTPHandler()

	for _, key := range sortedRouteKeys(spec) {
		method, path, _ := strings.Cut(key, " ")
		sec, hasSec := spec[key]["security"].([]any)
		isPublic := hasSec && len(sec) == 0
		if isPublic != publicRoutes[key] {
			t.Errorf("%s: the spec says public=%v but operatorPublic says %v", key, isPublic, publicRoutes[key])
		}
		// Fill path parameters with a placeholder and call without a credential.
		url := pathParam.ReplaceAllString(path, "x")
		if method == http.MethodGet && key == "GET /server-proof" {
			url += "?nonce=" + strings.Repeat("a", 32)
		}
		rec := operatorRequest(t, h, method, url, "", "")
		if isPublic && rec.Code == http.StatusUnauthorized {
			t.Errorf("%s: documented as public but answered 401", key)
		}
		if !isPublic && rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: documented as needing a credential but answered %d without one", key, rec.Code)
		}
	}
}
