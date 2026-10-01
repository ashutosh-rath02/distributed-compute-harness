package domain

import (
	"strings"
	"testing"
)

func TestValidArtifactName(t *testing.T) {
	good := []string{"data.txt", "in/part-01.csv", "a/b/c/d", "UPPER_lower-09.tar.gz", ".hidden", strings.Repeat("x", 64)}
	for _, n := range good {
		if err := ValidArtifactName(n); err != nil {
			t.Errorf("ValidArtifactName(%q) = %v, want ok", n, err)
		}
	}
	bad := []string{
		"", "/etc/passwd", "../x", "a/../../x", "a/./b", "./a", "a//b", "a/", `a\b`, `..\x`,
		"C:/x", "C:x", "a b", "a\x00b", "naïve.txt", "a/b/c/d/e", "x.", "dir./f",
		"con", "NUL.txt", "a/Com1.log", "lpt9", "aux.tar.gz",
		strings.Repeat("x", 65), strings.Repeat("a/", 64) + "b",
	}
	for _, n := range bad {
		if err := ValidArtifactName(n); err == nil {
			t.Errorf("ValidArtifactName(%q) = ok, want rejected", n)
		}
	}
}

func TestValidSHA256(t *testing.T) {
	if !ValidSHA256(strings.Repeat("ab", 32)) {
		t.Fatal("64 lowercase hex rejected")
	}
	for _, s := range []string{"", strings.Repeat("AB", 32), strings.Repeat("a", 63), strings.Repeat("g", 64), "../" + strings.Repeat("a", 61)} {
		if ValidSHA256(s) {
			t.Errorf("ValidSHA256(%q) = true", s)
		}
	}
}

func TestValidateWorkloadFilesRejectsCollisions(t *testing.T) {
	sha := strings.Repeat("0", 64)
	in := func(names ...string) []ArtifactRef {
		var out []ArtifactRef
		for _, n := range names {
			out = append(out, ArtifactRef{Name: n, SHA256: sha})
		}
		return out
	}
	if err := ValidateWorkloadFiles(in("a.txt", "dir/b.txt"), []string{"out/result.txt", "log.txt"}); err != nil {
		t.Fatalf("distinct names rejected: %v", err)
	}
	cases := []struct {
		name    string
		inputs  []ArtifactRef
		outputs []string
	}{
		{"duplicate input", in("a.txt", "a.txt"), nil},
		{"case-only difference", in("Data.txt"), []string{"data.TXT"}},
		{"input and output same file", in("x"), []string{"x"}},
		{"file is a directory of another", in("a"), []string{"a/b"}},
		{"prefix, case-insensitive", in("Dir/f"), []string{"dir"}},
		{"bad sha", []ArtifactRef{{Name: "a", SHA256: "nothex"}}, nil},
		{"bad name", in("../a"), nil},
		{"too many inputs", in(manyNames(MaxWorkloadInputs + 1)...), nil},
		{"too many outputs", nil, manyNames(MaxWorkloadOutputs + 1)},
	}
	for _, c := range cases {
		if err := ValidateWorkloadFiles(c.inputs, c.outputs); err == nil {
			t.Errorf("%s: accepted", c.name)
		}
	}
	// "ab" is not a directory of "a": only whole segments count.
	if err := ValidateWorkloadFiles(in("a"), []string{"ab/c"}); err != nil {
		t.Fatalf("segment-prefix false positive: %v", err)
	}
}

func manyNames(n int) []string {
	var out []string
	for i := 0; i < n; i++ {
		out = append(out, "f"+strings.Repeat("x", i))
	}
	return out
}
