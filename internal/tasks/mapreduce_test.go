package tasks

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

func mergeCountFiles() map[string][]byte {
	return map[string][]byte{
		"parts/0000/counts.json": []byte(`{"files":[{"name":"c0","lines":2}],"total":{"lines":2,"words":5,"bytes":30}}`),
		"parts/0001/counts.json": []byte(`{"files":[{"name":"c1","lines":3}],"total":{"lines":3,"words":4,"bytes":12}}`),
	}
}

func readOut(t *testing.T, env Env, i int) string {
	t.Helper()
	b, err := os.ReadFile(env.out(i))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func countOf(t *testing.T, name string, content string) (lines, words, bytes int64) {
	t.Helper()
	env, _, err := run(t, "text.count", nil, map[string][]byte{name: []byte(content)}, name)
	if err != nil {
		t.Fatal(err)
	}
	var got struct{ Total struct{ Lines, Words, Bytes int64 } }
	if err := json.Unmarshal([]byte(readOut(t, env, 0)), &got); err != nil {
		t.Fatal(err)
	}
	return got.Total.Lines, got.Total.Words, got.Total.Bytes
}

// The oracle: the chunks joined are the file, no chunk but the last
// ends mid-line, and the per-chunk counts add up to the whole file's,
// for both ways of cutting and for awkward files (no final newline,
// fewer lines than chunks, one huge line, empty).
func TestTextSplitChunksAddUpToTheWhole(t *testing.T) {
	inputs := map[string]string{
		"plain":       "alpha beta\ngamma\ndelta epsilon zeta\neta\n",
		"no newline":  "one\ntwo\nthree",
		"few lines":   "just one line here\n",
		"empty":       "",
		"huge line":   strings.Repeat("x", 300000) + "\nshort\n",
		"many":        strings.Repeat("the quick brown fox\n", 1001),
		"blank lines": "\n\n\na\n\n",
		"unicode":     "héllo wörld\n漢字 text\n",
	}
	for name, content := range inputs {
		for _, by := range []string{"lines", "bytes"} {
			for _, chunks := range []string{"1", "3", "16"} {
				t.Run(fmt.Sprintf("%s/%s/%s", name, by, chunks), func(t *testing.T) {
					env, _, err := run(t, "text.split", map[string]string{"chunks": chunks, "by": by}, map[string][]byte{"in.txt": []byte(content)}, "in.txt")
					if err != nil {
						t.Fatal(err)
					}
					var parts []string
					var l, w, b int64
					for i := range env.Outputs {
						c := readOut(t, env, i)
						parts = append(parts, c)
						cl, cw, cb := countOf(t, "c.txt", c)
						l, w, b = l+cl, w+cw, b+cb
					}
					if strings.Join(parts, "") != content {
						t.Fatal("chunks joined differ from the input")
					}
					for i, c := range parts {
						if c != "" && !strings.HasSuffix(c, "\n") && strings.Join(parts[i+1:], "") != "" {
							t.Fatalf("chunk %d ends mid-line", i+1)
						}
					}
					wl, ww, wb := countOf(t, "in.txt", content)
					if l != wl || w != ww || b != wb {
						t.Fatalf("chunk counts %d/%d/%d, whole %d/%d/%d", l, w, b, wl, ww, wb)
					}
				})
			}
		}
	}
}

func TestTextSplitNamesAndBalance(t *testing.T) {
	env, _, err := run(t, "text.split", map[string]string{"chunks": "3"}, map[string][]byte{"a.txt": []byte("x\n")}, "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(env.Outputs, ",") != "chunk-0001.txt,chunk-0002.txt,chunk-0003.txt" {
		t.Fatalf("outputs %v", env.Outputs)
	}
	for i := 1; i < 3; i++ {
		if readOut(t, env, i) != "" {
			t.Fatalf("chunk %d should be empty (fewer lines than chunks)", i+1)
		}
	}
	env, _, _ = run(t, "text.split", map[string]string{"chunks": "2"}, map[string][]byte{"a.txt": []byte("1\n2\n3\n4\n")}, "a.txt")
	if readOut(t, env, 0) != "1\n2\n" || readOut(t, env, 1) != "3\n4\n" {
		t.Fatalf("lines split: %q %q", readOut(t, env, 0), readOut(t, env, 1))
	}
}

func mergeRun(t *testing.T, params map[string]string, files map[string]string) (string, error) {
	t.Helper()
	var order []string
	in := map[string][]byte{}
	for n, c := range files {
		order = append(order, n)
		in[n] = []byte(c)
	}
	env, _, err := run(t, "json.merge", params, in, order...)
	if err != nil {
		return "", err
	}
	return readOut(t, env, 0), nil
}

func TestJSONMergeModes(t *testing.T) {
	counts := map[string]string{}
	for n, c := range mergeCountFiles() {
		counts[n] = string(c)
	}
	out, err := mergeRun(t, map[string]string{"mode": "sum", "field": "total"}, counts)
	if err != nil || !strings.Contains(out, `"lines": 5`) || !strings.Contains(out, `"words": 9`) || !strings.Contains(out, `"bytes": 42`) {
		t.Fatalf("sum: %v %s", err, out)
	}
	// Without a field: objects merge recursively, arrays join.
	out, err = mergeRun(t, map[string]string{"mode": "sum"}, counts)
	if err != nil || !strings.Contains(out, `"name": "c0"`) || !strings.Contains(out, `"name": "c1"`) || !strings.Contains(out, `"lines": 5`) {
		t.Fatalf("sum whole: %v %s", err, out)
	}
	out, err = mergeRun(t, map[string]string{"mode": "concat"}, map[string]string{"b.json": `[3]`, "a.json": `[1,2]`})
	if err != nil || strings.Join(strings.Fields(out), "") != "[1,2,3]" {
		t.Fatalf("concat (name order): %v %s", err, out)
	}
	out, err = mergeRun(t, map[string]string{"mode": "collect", "field": "n"}, map[string]string{"a.json": `{"n":1}`, "b.json": `{"n":{"x":2}}`})
	var got map[string]any
	if err != nil || json.Unmarshal([]byte(out), &got) != nil || len(got) != 2 || got["a.json"] == nil {
		t.Fatalf("collect: %v %s", err, out)
	}
	out, err = mergeRun(t, map[string]string{"mode": "sum"}, map[string]string{"a.json": `{"x":1.5}`, "b.json": `{"x":2}`})
	if err != nil || !strings.Contains(out, `"x": 3.5`) {
		t.Fatalf("floats: %v %s", err, out)
	}
}

func TestJSONMergeRefusals(t *testing.T) {
	sum := map[string]string{"mode": "sum"}
	cases := map[string]struct {
		params map[string]string
		files  map[string]string
		want   string
	}{
		"not json":             {sum, map[string]string{"a.json": `{`}, "not JSON"},
		"two values":           {sum, map[string]string{"a.json": `1 2`}, "more than one"},
		"concat of object":     {map[string]string{"mode": "concat"}, map[string]string{"a.json": `{}`}, "needs a JSON array"},
		"kinds differ":         {sum, map[string]string{"a.json": `{"x":1}`, "b.json": `{"x":"s"}`}, "can't add"},
		"text can't be summed": {sum, map[string]string{"a.json": `"s"`}, "can't add"},
		"overflow":             {sum, map[string]string{"a.json": `9223372036854775807`, "b.json": `1`}, "overflows"},
		"missing field":        {map[string]string{"mode": "sum", "field": "total"}, map[string]string{"a.json": `{}`}, "no field"},
		"too deep":             {map[string]string{"mode": "collect"}, map[string]string{"a.json": strings.Repeat("[", 40) + strings.Repeat("]", 40)}, "nested deeper"},
	}
	for name, c := range cases {
		if _, err := mergeRun(t, c.params, c.files); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
}
