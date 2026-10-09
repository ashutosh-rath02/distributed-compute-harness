package manager

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The pages are one big script each. A syntax error in it (a bad merge of
// two edits to one expression) leaves the whole page blank: no devices, no
// Approve button. Parse every page's script with Node when it is installed.
func TestPageScriptsParse(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	script := regexp.MustCompile(`(?s)<script>(.*?)</script>`)
	for name, page := range map[string]string{"dashboard.html": "dashboard.html", "live.html": "live.html"} {
		html, err := os.ReadFile(page)
		if err != nil {
			t.Fatal(err)
		}
		var js []string
		for _, m := range script.FindAllStringSubmatch(string(html), -1) {
			js = append(js, m[1])
		}
		if len(js) == 0 {
			t.Fatalf("%s has no script", name)
		}
		file := filepath.Join(t.TempDir(), name+".js")
		if err := os.WriteFile(file, []byte(strings.Join(js, "\n")), 0o600); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command(node, "--check", file).CombinedOutput(); err != nil {
			t.Errorf("%s: script doesn't parse:\n%s", name, out)
		}
	}
}
