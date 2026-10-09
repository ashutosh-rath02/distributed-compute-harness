package joinscript

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// windowsScripts is every PowerShell installer the harness can produce:
// join page, LAN invite, and the three relay forms.
func windowsScripts(t *testing.T) map[string]string {
	t.Helper()
	const fp = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	out := map[string]string{}
	var err error
	out["join-page"], err = BuildPairing("windows", PairingInfo{
		Fingerprint: fp, FallbackAddr: "192.168.0.8:7420",
		WindowsURL: "http://192.168.0.8:7419/agent/windows/amd64", WindowsSHA256: "abcd",
	})
	if err != nil {
		t.Fatal(err)
	}
	lan := Info{Fingerprint: "abc123", PairingToken: "tok", AgentBinaryAvailable: true, AgentBinarySHA256: "deadbeef"}
	if out["lan"], err = Build("192.168.0.8:7420", "windows", lan); err != nil {
		t.Fatal(err)
	}
	relay := Info{Insecure: true, AgentBinaryAvailable: true, AgentBinarySHA256: "deadbeef", PairingToken: "tok",
		RelayAvailable: true, RelayAddr: "relay.example.com:8420", RelayToken: "rt"}
	if out["relay-manual"], err = BuildMode(ModeRemote, "", "windows", relay); err != nil {
		t.Fatal(err)
	}
	relay.BootstrapURL = "https://relay.example.com/agent/windows"
	if out["relay-bootstrap"], err = BuildMode(ModeRemote, "", "windows", relay); err != nil {
		t.Fatal(err)
	}
	return out
}

// installerBody is the part of an invite's output that is the script itself
// (invites put one line of prose in front of it).
func installerBody(s string) string {
	if i := strings.Index(s, "$root = "); i >= 0 {
		return s[i:]
	}
	return s
}

func TestWindowsScriptsHaveNoHereStringsCRsOrBOM(t *testing.T) {
	for name, s := range windowsScripts(t) {
		body := installerBody(s)
		if strings.Contains(body, "@'") || strings.Contains(body, `@"`) {
			t.Errorf("%s: a here-string is in the script; iex fails on one when the response arrives line by line", name)
		}
		if strings.Contains(body, "\r") || strings.HasPrefix(body, "\xef\xbb\xbf") {
			t.Errorf("%s: CR or BOM in the script", name)
		}
	}
}

// parseWindowsPowerShell parses each script with the real PowerShell parser,
// twice: as one text (a saved file, or iex given the whole response) and
// line by line (iex runs each string piped to it separately, so a response
// that arrives split into lines must still parse). It returns the parser
// messages per script.
func parseWindowsPowerShell(t *testing.T, exe string, scripts map[string]string) map[string][]string {
	t.Helper()
	dir := t.TempDir()
	list := filepath.Join(dir, "list.txt")
	var names []string
	for name, s := range scripts {
		if err := os.WriteFile(filepath.Join(dir, name+".ps1"), []byte(installerBody(s)), 0o644); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := os.WriteFile(list, []byte(strings.Join(names, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	driver := filepath.Join(dir, "parse.ps1")
	const code = `param($dir)
foreach ($n in (Get-Content (Join-Path $dir 'list.txt'))) {
  $text = [IO.File]::ReadAllText((Join-Path $dir ($n + '.ps1')))
  $whole = $null; $err = $null
  [void][System.Management.Automation.Language.Parser]::ParseInput($text, [ref]$whole, [ref]$err)
  foreach ($e in $err) { Write-Output ($n + '|whole|' + $e.Message) }
  foreach ($line in ($text -split "` + "`n" + `")) {
    $err = $null
    [void][System.Management.Automation.Language.Parser]::ParseInput($line, [ref]$whole, [ref]$err)
    foreach ($e in $err) { Write-Output ($n + '|line|' + $e.Message + ' in: ' + $line.Substring(0, [Math]::Min(60, $line.Length))) }
  }
}
Write-Output 'done|done|done'
`
	if err := os.WriteFile(driver, []byte(code), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(exe, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", driver, dir).CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", exe, err, out)
	}
	res := map[string][]string{}
	sawDone := false
	for _, l := range strings.Split(strings.ReplaceAll(string(out), "\r", ""), "\n") {
		if l == "done|done|done" {
			sawDone = true
		} else if parts := strings.SplitN(l, "|", 2); len(parts) == 2 {
			res[parts[0]] = append(res[parts[0]], parts[1])
		}
	}
	if !sawDone {
		t.Fatalf("%s did not finish parsing:\n%s", exe, out)
	}
	return res
}

func TestWindowsScriptsParseInPowerShell(t *testing.T) {
	scripts := windowsScripts(t)
	ran := false
	for _, exe := range []string{"powershell.exe", "pwsh"} {
		path, err := exec.LookPath(exe)
		if err != nil {
			continue
		}
		ran = true
		res := parseWindowsPowerShell(t, path, scripts)
		for name := range scripts {
			for _, msg := range res[name] {
				t.Errorf("%s: %s: parse error: %s", exe, name, msg)
			}
		}
	}
	if !ran {
		t.Skip("no PowerShell installed")
	}
}
