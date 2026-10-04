package tasks

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// collect runs report.collect named name over files (name, content, ...)
// and returns the report.
func collect(t *testing.T, name string, files ...string) (string, error) {
	t.Helper()
	env, _, _ := typedEnv(t, "report.collect", map[string]string{"name": name}, files...)
	h, _ := Lookup("report.collect")
	if err := h.Run(context.Background(), env); err != nil {
		return "", err
	}
	data, err := os.ReadFile(env.out(0))
	return string(data), err
}

const threeTasks = `{"tasks":[{"key":"0000","name":"alpha.txt"},{"key":"0001","name":"beta notes.md"},{"key":"0002","name":"=sum(A1).txt"}]}`

// A job's parts are numbered, not named: the task index names each one
// after the file its task ran on.
func TestReportNamesSummariesByTheirFile(t *testing.T) {
	parts := []string{
		"parts/0000/response.txt", "  Alpha is about apples.\n",
		"parts/0001/response.txt", "Beta: bees | wasps",
		"parts/0002/response.txt", "-1 + 2",
		"parts/tasks.json", threeTasks,
	}
	md, err := collect(t, "summaries.md", parts...)
	if err != nil {
		t.Fatal(err)
	}
	if md != "## alpha.txt\n\nAlpha is about apples.\n\n## beta notes.md\n\nBeta: bees | wasps\n\n## =sum(A1).txt\n\n-1 + 2\n\n" {
		t.Fatalf("markdown:\n%s", md)
	}
	csv, err := collect(t, "summaries.csv", parts...)
	if err != nil {
		t.Fatal(err)
	}
	// Cells that would start a formula are defused.
	if csv != "file,text\nalpha.txt,Alpha is about apples.\nbeta notes.md,Beta: bees | wasps\n'=sum(A1).txt,'-1 + 2\n" {
		t.Fatalf("csv:\n%s", csv)
	}
	js, err := collect(t, "summaries.json", parts...)
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]string
	if err := json.Unmarshal([]byte(js), &rows); err != nil || len(rows) != 3 || rows[2]["file"] != "=sum(A1).txt" || rows[1]["text"] != "Beta: bees | wasps" {
		t.Fatalf("json %v:\n%s", err, js)
	}
}

func TestReportTablesLabels(t *testing.T) {
	parts := []string{
		"parts/0000/label.json", `{"file":"bill.txt","label":"invoice"}`,
		"parts/0001/label.json", `{"file":"a|b.txt","label":"letter to the bank"}`,
		"parts/tasks.json", threeTasks,
	}
	md, err := collect(t, "labels.md", parts...)
	if err != nil {
		t.Fatal(err)
	}
	if md != "| File | Label |\n| --- | --- |\n| bill.txt | invoice |\n| a\\|b.txt | letter to the bank |\n" {
		t.Fatalf("markdown:\n%s", md)
	}
	csv, err := collect(t, "labels.csv", parts...)
	if err != nil || csv != "file,label\nbill.txt,invoice\na|b.txt,letter to the bank\n" {
		t.Fatalf("csv %v:\n%s", err, csv)
	}
	// With text alongside, both columns.
	mixed, err := collect(t, "all.csv", append(parts, "parts/0002/response.txt", "text")...)
	if err != nil || !strings.HasPrefix(mixed, "file,label,text\nbill.txt,invoice,\n") || !strings.HasSuffix(mixed, "'=sum(A1).txt,,text\n") {
		t.Fatalf("mixed csv %v:\n%s", err, mixed)
	}
}

func TestReportJoinsEmbeddings(t *testing.T) {
	js, err := collect(t, "vectors.json",
		"parts/0000/embeddings.json", `[{"name":"a.txt","model":"e:latest","vector":[0.1,2]}]`,
		"parts/0001/embeddings.json", `[{"name":"b.txt","model":"e:latest","vector":[3,-0.5]},{"name":"c.txt","model":"e:latest","vector":[1,1]}]`,
		"parts/tasks.json", threeTasks)
	if err != nil {
		t.Fatal(err)
	}
	var got []Embedding
	if err := json.Unmarshal([]byte(js), &got); err != nil || len(got) != 3 || got[0].Name != "a.txt" || got[2].Name != "c.txt" || got[1].Vector[1] != -0.5 {
		t.Fatalf("%v %+v:\n%s", err, got, js)
	}
	if !strings.Contains(js, `"vector":[0.1,2]`) {
		t.Fatalf("numbers changed:\n%s", js)
	}
	for name, c := range map[string]struct {
		report string
		files  []string
		want   string
	}{
		"into markdown":  {"v.md", []string{"parts/0000/embeddings.json", `[]`}, "into a .json report"},
		"mixed":          {"v.json", []string{"parts/0000/embeddings.json", `[]`, "parts/0001/label.json", `{"file":"a","label":"b"}`}, "can't share"},
		"no vector":      {"v.json", []string{"parts/0000/embeddings.json", `[{"name":"a.txt","vector":[]}]`}, "without a name or numbers"},
		"not embeddings": {"v.json", []string{"parts/0000/embeddings.json", `{"a":1}`}, "isn't embeddings"},
		"bad label":      {"l.csv", []string{"parts/0000/label.json", `{"file":"a"}`}, "isn't a label"},
		"an image":       {"r.md", []string{"parts/0000/out.json", `{}`}, "can't collect parts/0000/out.json"},
		"only the index": {"r.md", []string{"parts/tasks.json", threeTasks}, "no results"},
	} {
		if _, err := collect(t, c.report, c.files...); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
}

// Without a job (or a task without a name) rows still get a name; a task
// that left several results names each.
func TestReportNamesWithoutAnIndex(t *testing.T) {
	md, err := collect(t, "r.md",
		"notes/a.txt", "from a file given directly",
		"parts/0003/response.txt", "an unnamed task",
		"parts/0004/out.txt", "first", "parts/0004/err.txt", "second",
		"parts/tasks.json", `{"tasks":[{"key":"0004","name":"x.txt"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"## notes/a.txt\n\nfrom a file given directly", "## task 0003\n\nan unnamed task", "## x.txt (out.txt)\n\nfirst", "## x.txt (err.txt)\n\nsecond"} {
		if !strings.Contains(md, want) {
			t.Errorf("missing %q in:\n%s", want, md)
		}
	}
	// A name can't break the Markdown's lines.
	md, err = collect(t, "r.md", "parts/0000/response.txt", "x", "parts/tasks.json", `{"tasks":[{"key":"0000","name":"two\nlines"}]}`)
	if err != nil || !strings.HasPrefix(md, "## two lines\n") {
		t.Fatalf("%v:\n%s", err, md)
	}
}
