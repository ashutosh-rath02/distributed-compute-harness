package main

import (
	"os"
	"strings"
	"testing"

	"home-harness/internal/catalog"
)

func inRepoRoot(t *testing.T) {
	t.Helper()
	old, _ := os.Getwd()
	if err := os.Chdir("../.."); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(old) })
}

func TestTaskReferenceCoversTheCatalog(t *testing.T) {
	body := genTasks()
	for _, ty := range catalog.Types() {
		has := strings.Contains(body, `id="`+anchor(string(ty.Name))+`"`)
		if ty.Internal && has {
			t.Errorf("internal type %s must not be documented as a task", ty.Name)
		}
		if !ty.Internal && !has {
			t.Errorf("task type %s is missing from the generated reference", ty.Name)
		}
	}
}

func TestMarkdownEscapesHTMLAndBlocksScriptLinks(t *testing.T) {
	out := markdown("Hello <script>alert(1)</script> and [x](javascript:alert(1)) and `<b>`\n\n```\n<img src=x onerror=1>\n```\n")
	for _, bad := range []string{"<script>", "javascript:", "<img", "<b>"} {
		if strings.Contains(out, bad) {
			t.Errorf("output contains %q:\n%s", bad, out)
		}
	}
}

func TestMarkdownBasics(t *testing.T) {
	registerLinks()
	out := markdown("# Title\n\nSome **bold** and *em* text with [a link](quickstart.md#top).\n\n- one\n- two\n\n| a | b |\n|---|---|\n| 1 | 2 |\n")
	for _, want := range []string{`<h1 id="title">Title</h1>`, "<strong>bold</strong>", "<em>em</em>", `href="quickstart.html#top"`, "<li>one</li>", "<td>2</td>"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestEveryPageRendersAndLinksResolve(t *testing.T) {
	inRepoRoot(t)
	known := registerLinks()
	for _, p := range pages {
		body, _ := p.render()
		if strings.TrimSpace(body) == "" {
			t.Errorf("%s renders empty", p.slug)
		}
		for _, l := range internalLinks(body) {
			if !p.linked && !known[strings.SplitN(l, "#", 2)[0]] {
				t.Errorf("%s links to %s, which is not a page", p.slug, l)
			}
		}
	}
}

func TestAPIPageListsEveryOperation(t *testing.T) {
	inRepoRoot(t)
	body := genAPI()
	for _, want := range []string{"<code>GET</code></td><td><code>/nodes</code>", "<code>POST</code></td><td><code>/workloads</code>", "<code>POST</code></td><td><code>/v1/chat/completions</code>"} {
		if !strings.Contains(body, want) {
			t.Errorf("API page is missing %s", want)
		}
	}
	if strings.Count(body, "<tr>") < 60 {
		t.Errorf("API page lists only %d rows", strings.Count(body, "<tr>"))
	}
}
