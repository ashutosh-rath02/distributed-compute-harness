// Command build turns the Markdown in docs/ into a small static site in
// docs/site/: no dependencies, no JavaScript needed to read it, light and dark
// themes, readable on a phone.
//
//	go run ./docs/build            from the repository root
//	go run ./docs/build -check     build into a temp folder and fail on broken links (for tests/CI)
//
// The task reference is generated from the catalog the manager itself uses
// (internal/catalog), and the API page from docs/openapi.yaml, so neither
// can go stale. Pages other people write (operations, observability, ...) are
// included when their files exist and shown as "not written yet" when not.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"home-harness/internal/catalog"
)

// page is one page of the site.
type page struct {
	slug   string // output file name without .html
	title  string
	src    string // Markdown file, relative to docs/ ("" for generated pages)
	group  string
	gen    func() string // generated body (HTML), when src == ""
	linked bool          // written by someone else: tolerate a missing file
}

var pages = []page{
	{slug: "index", title: "Home", src: "src/index.md", group: "Start"},
	{slug: "quickstart", title: "Quickstart", src: "src/quickstart.md", group: "Start"},
	{slug: "concepts", title: "Concepts", src: "src/concepts.md", group: "Start"},
	{slug: "tasks", title: "Task types", group: "Reference", gen: genTasks},
	{slug: "api", title: "HTTP API", group: "Reference", gen: genAPI},
	{slug: "clients", title: "Client libraries", src: "src/clients.md", group: "Reference"},
	{slug: "operations", title: "Operations", src: "operations.md", group: "Run it", linked: true},
	{slug: "observability", title: "Observability", src: "observability.md", group: "Run it", linked: true},
	{slug: "backup-restore", title: "Backup and restore", src: "backup-restore.md", group: "Run it", linked: true},
	{slug: "workflows", title: "Workflows", src: "workflows.md", group: "Run it", linked: true},
	{slug: "schedules", title: "Schedules", src: "schedules.md", group: "Run it", linked: true},
	{slug: "uninstall", title: "Uninstall", src: "uninstall.md", group: "Run it"},
	{slug: "security", title: "Security overview", src: "src/security.md", group: "Trust"},
	{slug: "threat-model", title: "Threat model", src: "THREAT_MODEL.md", group: "Trust", linked: true},
	{slug: "troubleshooting", title: "Troubleshooting", src: "src/troubleshooting.md", group: "Help"},
	{slug: "faq", title: "FAQ", src: "src/faq.md", group: "Help"},
	{slug: "compare", title: "Compared with others", src: "src/compare.md", group: "Help"},
}

// mdLinks maps a Markdown file name to its page, so links between docs work.
var mdLinks = map[string]string{
	"THREAT_MODEL.md": "threat-model.html",
}

func main() {
	check := flag.Bool("check", false, "build into a temporary folder and report broken internal links")
	out := flag.String("out", "docs/site", "output folder")
	flag.Parse()
	if _, err := os.Stat("docs/openapi.yaml"); err != nil {
		fatal("run this from the repository root (docs/openapi.yaml not found)")
	}
	dest := *out
	if *check {
		d, err := os.MkdirTemp("", "docs-site-")
		if err != nil {
			fatal(err.Error())
		}
		defer os.RemoveAll(d)
		dest = d
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		fatal(err.Error())
	}
	known := registerLinks()
	var broken []string
	for _, p := range pages {
		body, missing := p.render()
		for _, l := range internalLinks(body) {
			if !known[strings.SplitN(l, "#", 2)[0]] {
				if p.linked {
					// Written by someone else, and free to link to repository files the site does not carry.
					fmt.Printf("warning: %s links to %s, which is not a page of this site\n", p.slug, l)
					continue
				}
				broken = append(broken, p.slug+": "+l)
			}
		}
		if missing {
			fmt.Printf("note: %s is not written yet; the page says so\n", p.src)
		}
		if err := os.WriteFile(filepath.Join(dest, p.slug+".html"), []byte(layout(p, body)), 0o644); err != nil {
			fatal(err.Error())
		}
	}
	spec, err := os.ReadFile("docs/openapi.yaml")
	if err != nil {
		fatal(err.Error())
	}
	if err := os.WriteFile(filepath.Join(dest, "openapi.yaml"), spec, 0o644); err != nil {
		fatal(err.Error())
	}
	if len(broken) > 0 {
		fatal("broken links:\n  " + strings.Join(broken, "\n  "))
	}
	if !*check {
		fmt.Printf("wrote %d pages to %s\n", len(pages), dest)
	}
}

// registerLinks lets Markdown link to a page as "tasks.md" or by its source
// file name ("THREAT_MODEL.md"), and returns the set of output files.
func registerLinks() map[string]bool {
	known := map[string]bool{"openapi.yaml": true}
	for _, p := range pages {
		known[p.slug+".html"] = true
		mdLinks[p.slug+".md"] = p.slug + ".html"
		if p.src != "" {
			mdLinks[filepath.Base(p.src)] = p.slug + ".html"
		}
	}
	return known
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "docs/build:", msg)
	os.Exit(1)
}

// render returns the page body (HTML) and whether a linked page was missing.
func (p page) render() (string, bool) {
	if p.gen != nil {
		return p.gen(), false
	}
	raw, err := os.ReadFile(filepath.Join("docs", filepath.FromSlash(p.src)))
	if err != nil {
		if p.linked {
			return "<h1>" + html.EscapeString(p.title) + "</h1>\n<p class=\"note\">This page is not written yet. When <code>docs/" +
				html.EscapeString(p.src) + "</code> exists, the next build includes it here.</p>\n", true
		}
		fatal("missing " + p.src)
	}
	return markdown(string(raw)), false
}

var linkRe = regexp.MustCompile(`href="([^":/#][^":]*)"`)

func internalLinks(body string) []string {
	var out []string
	for _, m := range linkRe.FindAllStringSubmatch(body, -1) {
		out = append(out, m[1])
	}
	return out
}

// ---- layout

func layout(cur page, body string) string {
	var nav strings.Builder
	group := ""
	for _, p := range pages {
		if p.group != group {
			if group != "" {
				nav.WriteString("</ul>\n")
			}
			group = p.group
			nav.WriteString("<h2>" + html.EscapeString(group) + "</h2>\n<ul>\n")
		}
		cls := ""
		if p.slug == cur.slug {
			cls = ` aria-current="page"`
		}
		nav.WriteString(`<li><a href="` + p.slug + `.html"` + cls + ">" + html.EscapeString(p.title) + "</a></li>\n")
	}
	nav.WriteString("</ul>\n")
	return strings.NewReplacer("{{TITLE}}", html.EscapeString(cur.title), "{{NAV}}", nav.String(), "{{BODY}}", body).Replace(shell)
}

const shell = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{TITLE}} - Home Harness</title>
<style>
:root{--bg:#fbfbfa;--fg:#1d2024;--muted:#5b6470;--line:#dcdfe3;--panel:#f1f2f4;--accent:#1f5fbf;--code:#eceef1;--warn:#fff4d6;--warnline:#e5c24d}
@media (prefers-color-scheme:dark){:root:not([data-theme=light]){--bg:#14161a;--fg:#e6e8eb;--muted:#9aa3ae;--line:#2c3138;--panel:#1b1e23;--accent:#7db3ff;--code:#22262c;--warn:#3a3217;--warnline:#8a7424}}
:root[data-theme=dark]{--bg:#14161a;--fg:#e6e8eb;--muted:#9aa3ae;--line:#2c3138;--panel:#1b1e23;--accent:#7db3ff;--code:#22262c;--warn:#3a3217;--warnline:#8a7424}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--fg);font:16px/1.6 system-ui,-apple-system,"Segoe UI",Roboto,sans-serif}
a{color:var(--accent)}
header{display:flex;align-items:center;gap:12px;padding:10px 16px;border-bottom:1px solid var(--line);position:sticky;top:0;background:var(--bg);z-index:2}
header .brand{font-weight:650;color:var(--fg);text-decoration:none}
header button{margin-left:auto;background:var(--panel);color:var(--fg);border:1px solid var(--line);border-radius:6px;padding:6px 10px;font:inherit;cursor:pointer}
#menu{display:none}
.wrap{display:flex;max-width:1100px;margin:0 auto}
nav{width:230px;flex:none;padding:16px;border-right:1px solid var(--line);font-size:15px}
nav h2{font-size:12px;text-transform:uppercase;letter-spacing:.06em;color:var(--muted);margin:18px 0 4px}
nav ul{list-style:none;margin:0;padding:0}
nav a{display:block;padding:3px 0;color:var(--fg);text-decoration:none}
nav a[aria-current=page]{color:var(--accent);font-weight:600}
main{flex:1;min-width:0;padding:8px 24px 64px}
h1{font-size:2rem;line-height:1.2;margin:1.2rem 0 .6rem}h2{margin-top:2rem;border-bottom:1px solid var(--line);padding-bottom:.2rem}h3{margin-top:1.5rem}
code{background:var(--code);padding:.1em .35em;border-radius:4px;font:0.9em ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}
pre{background:var(--code);padding:12px 14px;border-radius:8px;overflow:auto;line-height:1.45}pre code{background:none;padding:0}
table{border-collapse:collapse;display:block;overflow-x:auto;max-width:100%}th,td{border:1px solid var(--line);padding:6px 10px;text-align:left;vertical-align:top}th{background:var(--panel)}
blockquote{margin:1rem 0;padding:.6rem 1rem;background:var(--warn);border-left:4px solid var(--warnline);border-radius:4px}blockquote p{margin:.3rem 0}
.note{background:var(--panel);padding:.6rem 1rem;border-radius:6px;border:1px solid var(--line)}
.tag{display:inline-block;font-size:.75rem;padding:0 .45rem;border:1px solid var(--line);border-radius:999px;color:var(--muted);margin-left:.3rem}
@media (max-width:760px){
  #menu{display:inline-block}
  .wrap{display:block}
  nav{display:none;width:auto;border-right:0;border-bottom:1px solid var(--line)}
  nav.open{display:block}
  main{padding:8px 16px 48px}
}
</style>
<script>
(function(){try{var t=localStorage.getItem('hh-theme');if(t)document.documentElement.setAttribute('data-theme',t)}catch(e){}})();
</script>
</head>
<body>
<header>
<button id="menu" type="button" aria-label="Menu" onclick="document.querySelector('nav').classList.toggle('open')">Menu</button>
<a class="brand" href="index.html">Home Harness</a>
<button type="button" onclick="(function(){var r=document.documentElement,d=r.getAttribute('data-theme');if(!d)d=matchMedia('(prefers-color-scheme: dark)').matches?'dark':'light';var n=d==='dark'?'light':'dark';r.setAttribute('data-theme',n);try{localStorage.setItem('hh-theme',n)}catch(e){}})()">Light / dark</button>
</header>
<div class="wrap">
<nav>
{{NAV}}</nav>
<main>
{{BODY}}</main>
</div>
</body>
</html>
`

// ---- generated pages

func genTasks() string {
	var b strings.Builder
	b.WriteString("<h1>Task types</h1>\n<p class=\"note\">Generated from the catalog the manager and agents are built with, so it is always what this version accepts. Every parameter value is sent as a string. Your operator policy decides which types are switched on (<code>harnessctl policy</code>).</p>\n")
	types := catalog.Types()
	sort.Slice(types, func(i, j int) bool { return types[i].Name < types[j].Name })
	b.WriteString("<ul>\n")
	for _, t := range types {
		if t.Internal {
			continue
		}
		b.WriteString(`<li><a href="#` + anchor(string(t.Name)) + `"><code>` + html.EscapeString(string(t.Name)) + "</code></a>: " + html.EscapeString(t.Title) + "</li>\n")
	}
	b.WriteString("</ul>\n")
	for _, t := range types {
		if t.Internal {
			continue
		}
		b.WriteString(`<h2 id="` + anchor(string(t.Name)) + `"><code>` + html.EscapeString(string(t.Name)) + "</code>")
		for _, tag := range tags(t) {
			b.WriteString(`<span class="tag">` + html.EscapeString(tag) + "</span>")
		}
		b.WriteString("</h2>\n<p><strong>" + html.EscapeString(t.Title) + ".</strong> " + html.EscapeString(t.Description) + "</p>\n")
		if len(t.Params) > 0 {
			b.WriteString("<table>\n<tr><th>Parameter</th><th>Type</th><th>Required</th><th>Default</th><th>Meaning and limits</th></tr>\n")
			for _, p := range t.Params {
				req := ""
				if p.Required {
					req = "yes"
				}
				b.WriteString("<tr><td><code>" + html.EscapeString(p.Name) + "</code></td><td>" + html.EscapeString(string(p.Type)) + "</td><td>" + req + "</td><td>" +
					html.EscapeString(p.Default) + "</td><td>" + html.EscapeString(paramNotes(p)) + "</td></tr>\n")
			}
			b.WriteString("</table>\n")
		} else {
			b.WriteString("<p>No parameters.</p>\n")
		}
		b.WriteString("<p>" + inputsText(t) + "</p>\n")
		if len(t.Outputs) > 0 {
			b.WriteString("<p>Result files: <code>" + html.EscapeString(strings.Join(t.Outputs, "</code>, <code>")) + "</code>.</p>\n")
		}
	}
	return b.String()
}

func tags(t catalog.Type) []string {
	var out []string
	if t.OptIn {
		out = append(out, "off until policy enables it")
	}
	if t.Reduce {
		out = append(out, "can combine results")
	}
	if t.Parts {
		out = append(out, "runs in parts")
	}
	if t.TargetRequired {
		out = append(out, "names a device")
	}
	if t.Streams {
		out = append(out, "streams output")
	}
	return out
}

func paramNotes(p catalog.Param) string {
	var n []string
	if p.Title != "" {
		n = append(n, p.Title)
	}
	if p.Description != "" {
		n = append(n, p.Description)
	}
	if len(p.Enum) > 0 {
		n = append(n, "one of: "+strings.Join(p.Enum, ", "))
	}
	if p.Min != nil {
		n = append(n, fmt.Sprintf("at least %g", *p.Min))
	}
	if p.Max != nil {
		n = append(n, fmt.Sprintf("at most %g", *p.Max))
	}
	if p.MaxLength > 0 {
		n = append(n, fmt.Sprintf("up to %d characters", p.MaxLength))
	}
	if p.Pattern != "" {
		n = append(n, "pattern "+p.Pattern)
	}
	return strings.Join(n, ". ")
}

func inputsText(t catalog.Type) string {
	in := t.Inputs
	var s string
	switch {
	case in.Max == 0:
		s = "Takes no input files."
	case in.Min == in.Max:
		s = fmt.Sprintf("Takes exactly %d input file(s).", in.Min)
	default:
		s = fmt.Sprintf("Takes %d to %d input files.", in.Min, in.Max)
	}
	if len(in.Extensions) > 0 {
		s += " File types: " + html.EscapeString(strings.Join(in.Extensions, ", ")) + "."
	}
	if in.Description != "" {
		s += " " + html.EscapeString(in.Description)
	}
	return s
}

var nonAnchor = regexp.MustCompile(`[^a-z0-9]+`)

func anchor(s string) string { return strings.Trim(nonAnchor.ReplaceAllString(strings.ToLower(s), "-"), "-") }

// genAPI lists the routes from docs/openapi.yaml with a plain line scan: the
// spec is written in a regular layout (paths at 2 spaces, methods at 4,
// summary at 6) that a test keeps well formed.
func genAPI() string {
	f, err := os.Open("docs/openapi.yaml")
	if err != nil {
		fatal(err.Error())
	}
	defer f.Close()
	type op struct{ method, path, summary, tag string }
	var ops []op
	inPaths := false
	path, method := "", ""
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "paths:") {
			inPaths = true
			continue
		}
		if inPaths && line != "" && !strings.HasPrefix(line, " ") {
			break
		}
		if !inPaths {
			continue
		}
		switch {
		case strings.HasPrefix(line, "  /") && strings.HasSuffix(line, ":"):
			path, method = strings.TrimSuffix(strings.TrimSpace(line), ":"), ""
		case len(line) > 4 && strings.HasPrefix(line, "    ") && !strings.HasPrefix(line, "     ") && strings.HasSuffix(line, ":"):
			method = strings.ToUpper(strings.TrimSuffix(strings.TrimSpace(line), ":"))
			ops = append(ops, op{method: method, path: path})
		case strings.HasPrefix(line, "      summary:") && len(ops) > 0:
			ops[len(ops)-1].summary = strings.Trim(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "summary:")), `"'`)
		case strings.HasPrefix(line, "      tags:") && len(ops) > 0:
			t := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "tags:"))
			ops[len(ops)-1].tag = strings.Trim(t, "[]")
		}
	}
	var b strings.Builder
	b.WriteString(`<h1>HTTP API</h1>
<p>The manager's operator API is described in <a href="openapi.yaml">openapi.yaml</a> (OpenAPI 3.1): every route, its request and response shapes, and how it authenticates. A test fails the build if a route exists that the file does not describe. This page lists the routes from that file.</p>
<p>Every route needs <code>Authorization: Bearer &lt;operator token&gt;</code>, except a few public ones noted in the file; the <code>/v1</code> routes also accept the AI key. Most errors are a short plain-text message that says what to do next.</p>
<table>
<tr><th>Method</th><th>Path</th><th>What it does</th><th>Area</th></tr>
`)
	for _, o := range ops {
		b.WriteString("<tr><td><code>" + o.method + "</code></td><td><code>" + html.EscapeString(o.path) + "</code></td><td>" + html.EscapeString(o.summary) + "</td><td>" + html.EscapeString(o.tag) + "</td></tr>\n")
	}
	b.WriteString("</table>\n")
	return b.String()
}

// ---- Markdown (a small subset: headings, paragraphs, lists, code, tables,
// quotes, emphasis, links). Raw HTML in the source is escaped, never passed on.

var (
	codeSpan = regexp.MustCompile("`([^`]+)`")
	boldRe   = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	emRe     = regexp.MustCompile(`(^|[^*\w])\*([^*\s][^*]*)\*`)
	linkMd   = regexp.MustCompile(`\[([^\]]+)\]\(([^)\s]+)\)`)
	olRe     = regexp.MustCompile(`^\d+\.\s+`)
)

func inline(s string) string {
	// Code spans first, so their content is not touched by the rest.
	var spans []string
	s = codeSpan.ReplaceAllStringFunc(s, func(m string) string {
		spans = append(spans, "<code>"+html.EscapeString(m[1:len(m)-1])+"</code>")
		return fmt.Sprintf("\x00%d\x00", len(spans)-1)
	})
	s = html.EscapeString(s)
	s = linkMd.ReplaceAllStringFunc(s, func(m string) string {
		g := linkMd.FindStringSubmatch(m)
		return `<a href="` + linkTarget(g[2]) + `">` + g[1] + "</a>"
	})
	s = boldRe.ReplaceAllString(s, "<strong>$1</strong>")
	s = emRe.ReplaceAllString(s, "$1<em>$2</em>")
	for i, sp := range spans {
		s = strings.Replace(s, fmt.Sprintf("\x00%d\x00", i), sp, 1)
	}
	return s
}

func linkTarget(t string) string {
	t = html.UnescapeString(t)
	lower := strings.ToLower(t)
	if strings.HasPrefix(lower, "javascript:") || strings.HasPrefix(lower, "data:") || strings.HasPrefix(lower, "vbscript:") {
		return "#"
	}
	base, frag, _ := strings.Cut(t, "#")
	if strings.Contains(base, "://") || strings.HasPrefix(base, "mailto:") {
		return html.EscapeString(t)
	}
	if to, ok := mdLinks[filepath.Base(base)]; ok && strings.HasSuffix(base, ".md") {
		base = to
	}
	if frag != "" {
		base += "#" + frag
	}
	return html.EscapeString(base)
}

func markdown(src string) string {
	lines := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")
	var b strings.Builder
	i := 0
	para := []string{}
	flush := func() {
		if len(para) > 0 {
			b.WriteString("<p>" + inline(strings.Join(para, " ")) + "</p>\n")
			para = para[:0]
		}
	}
	for i < len(lines) {
		line := lines[i]
		trim := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trim, "```"):
			flush()
			i++
			var code []string
			for i < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[i]), "```") {
				code = append(code, lines[i])
				i++
			}
			i++
			b.WriteString("<pre><code>" + html.EscapeString(strings.Join(code, "\n")) + "</code></pre>\n")
			continue
		case trim == "":
			flush()
		case strings.HasPrefix(trim, "#"):
			flush()
			n := len(trim) - len(strings.TrimLeft(trim, "#"))
			if n > 6 {
				n = 6
			}
			text := strings.TrimSpace(trim[n:])
			b.WriteString(fmt.Sprintf(`<h%d id="%s">%s</h%d>`+"\n", n, anchor(text), inline(text), n))
		case strings.HasPrefix(trim, "> "), trim == ">":
			flush()
			var q []string
			for i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), ">") {
				q = append(q, strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lines[i]), ">")))
				i++
			}
			b.WriteString("<blockquote><p>" + inline(strings.Join(q, " ")) + "</p></blockquote>\n")
			continue
		case strings.HasPrefix(trim, "|") && i+1 < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i+1]), "|") && strings.Contains(lines[i+1], "---"):
			flush()
			b.WriteString("<table>\n<tr>")
			for _, c := range cells(trim) {
				b.WriteString("<th>" + inline(c) + "</th>")
			}
			b.WriteString("</tr>\n")
			i += 2
			for i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), "|") {
				b.WriteString("<tr>")
				for _, c := range cells(strings.TrimSpace(lines[i])) {
					b.WriteString("<td>" + inline(c) + "</td>")
				}
				b.WriteString("</tr>\n")
				i++
			}
			b.WriteString("</table>\n")
			continue
		case strings.HasPrefix(trim, "- ") || strings.HasPrefix(trim, "* ") || olRe.MatchString(trim):
			flush()
			ordered := olRe.MatchString(trim)
			tag := "ul"
			if ordered {
				tag = "ol"
			}
			b.WriteString("<" + tag + ">\n")
			for i < len(lines) {
				t := strings.TrimSpace(lines[i])
				var item string
				switch {
				case !ordered && (strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "* ")):
					item = t[2:]
				case ordered && olRe.MatchString(t):
					item = olRe.ReplaceAllString(t, "")
				default:
					item = ""
				}
				if item == "" {
					break
				}
				i++
				// Continuation lines (indented, not a new item).
				for i < len(lines) && strings.HasPrefix(lines[i], "  ") && strings.TrimSpace(lines[i]) != "" &&
					!strings.HasPrefix(strings.TrimSpace(lines[i]), "- ") && !olRe.MatchString(strings.TrimSpace(lines[i])) {
					item += " " + strings.TrimSpace(lines[i])
					i++
				}
				b.WriteString("<li>" + inline(item) + "</li>\n")
			}
			b.WriteString("</" + tag + ">\n")
			continue
		default:
			para = append(para, trim)
		}
		i++
	}
	flush()
	return b.String()
}

func cells(row string) []string {
	row = strings.Trim(row, "|")
	parts := strings.Split(row, "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}
