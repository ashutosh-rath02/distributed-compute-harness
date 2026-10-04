package tasks

import (
	"bufio"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"unicode"

	"home-harness/internal/catalog"
)

// ---- report.collect: a job's per-file results joined into one file, by
// the file each came from. As a job's reduce it gets every task's outputs
// at parts/<task>/<name> and, being a PartNames type, catalog.TaskIndexName
// with each task's name; given plain files instead, each row is named
// after its file.

type reportCollect struct{}

func (reportCollect) Available(ctx context.Context) error { return always(ctx) }

// maxReportPart bounds one result read into memory: a summary is a few
// KB, a file's embeddings well under a MB.
const maxReportPart = 8 << 20

// partKind is what a result holds, told by its name.
type partKind int

const (
	partText       partKind = iota // an answer or summary (.txt, .md)
	partLabel                      // llm.classify's label.json
	partEmbeddings                 // llm.embed's embeddings.json
)

type reportPart struct {
	input int    // index into the inputs
	group string // "task:<key>" for a job's part, "file:<name>" otherwise
	out   string // the result's name within its task (or the file's)
	kind  partKind
}

func (reportCollect) Run(ctx context.Context, env Env) error {
	format := strings.TrimPrefix(path.Ext(env.Outputs[0]), ".")
	names := map[string]string{} // task key -> its name
	var parts []reportPart
	perGroup := map[string]int{}
	var hasText, hasLabel, hasEmbeddings bool
	for i, in := range env.Inputs {
		if in == catalog.TaskIndexName {
			if err := readTaskIndex(env.in(i), names); err != nil {
				return err
			}
			continue
		}
		p := reportPart{input: i, group: "file:" + in, out: in}
		if rest, ok := strings.CutPrefix(in, "parts/"); ok {
			if key, out, ok := strings.Cut(rest, "/"); ok {
				p.group, p.out = "task:"+key, out
			}
		}
		switch base := strings.ToLower(path.Base(p.out)); {
		case base == "label.json":
			p.kind, hasLabel = partLabel, true
		case base == "embeddings.json":
			p.kind, hasEmbeddings = partEmbeddings, true
		case strings.HasSuffix(base, ".txt"), strings.HasSuffix(base, ".md"):
			p.kind, hasText = partText, true
		default:
			return fmt.Errorf("can't collect %s: results are text (.txt, .md), label.json or embeddings.json", in)
		}
		parts = append(parts, p)
		perGroup[p.group]++
	}
	switch {
	case len(parts) == 0:
		return errors.New("no results to collect")
	case hasEmbeddings && (hasText || hasLabel):
		return errors.New("embeddings can't share a report with other results")
	case hasEmbeddings && format != "json":
		return fmt.Errorf("embeddings join into a .json report, not .%s", format)
	}
	// nameOf names a result: the file it came from (its task's name in a
	// job), with the result's own name when its task left several.
	nameOf := func(p reportPart) string {
		key, inJob := strings.CutPrefix(p.group, "task:")
		if !inJob {
			return p.out
		}
		name := names[key]
		if name == "" {
			name = "task " + key
		}
		if perGroup[p.group] > 1 {
			name += " (" + p.out + ")"
		}
		return name
	}

	f, err := os.Create(env.out(0))
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	var cw *csv.Writer
	switch format {
	case "md":
		if !hasText {
			w.WriteString("| File | Label |\n| --- | --- |\n")
		}
	case "csv":
		cw = csv.NewWriter(w)
		header := []string{"file"}
		if hasLabel {
			header = append(header, "label")
		}
		if hasText {
			header = append(header, "text")
		}
		cw.Write(header)
	case "json":
		w.WriteString("[\n")
	}
	rows := 0
	for _, p := range parts {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := env.Inputs[p.input]
		data, err := readPart(env.in(p.input), name)
		if err != nil {
			return err
		}
		var file, label, text string
		switch p.kind {
		case partEmbeddings:
			var entries []Embedding
			if err := json.Unmarshal(data, &entries); err != nil {
				return fmt.Errorf("%s isn't embeddings from llm.embed: %v", name, err)
			}
			for _, e := range entries {
				if e.Name == "" || len(e.Vector) == 0 {
					return fmt.Errorf("%s has an embedding without a name or numbers", name)
				}
				line, _ := json.Marshal(e)
				if rows > 0 {
					w.WriteString(",\n")
				}
				w.Write(line)
				rows++
			}
			continue
		case partLabel:
			var l struct {
				File  string `json:"file"`
				Label string `json:"label"`
			}
			if json.Unmarshal(data, &l) != nil || l.Label == "" {
				return fmt.Errorf("%s isn't a label from llm.classify", name)
			}
			file, label = l.File, l.Label
			if file == "" {
				file = nameOf(p)
			}
		case partText:
			file, text = nameOf(p), strings.TrimSpace(strings.ToValidUTF8(string(data), "�"))
		}
		switch format {
		case "md":
			switch {
			case !hasText:
				fmt.Fprintf(w, "| %s | %s |\n", mdCell(file), mdCell(label))
			case label != "":
				fmt.Fprintf(w, "## %s\n\nLabel: **%s**\n\n", mdLine(file), mdCell(label))
			default:
				if text == "" {
					text = "(empty)"
				}
				fmt.Fprintf(w, "## %s\n\n%s\n\n", mdLine(file), text)
			}
		case "csv":
			row := []string{csvCell(file)}
			if hasLabel {
				row = append(row, csvCell(label))
			}
			if hasText {
				row = append(row, csvCell(text))
			}
			cw.Write(row)
		case "json":
			entry := map[string]string{"file": file}
			if p.kind == partLabel {
				entry["label"] = label
			} else {
				entry["text"] = text
			}
			line, _ := json.Marshal(entry)
			if rows > 0 {
				w.WriteString(",\n")
			}
			w.Write(line)
		}
		rows++
	}
	switch format {
	case "csv":
		cw.Flush()
		if err := cw.Error(); err != nil {
			return err
		}
	case "json":
		w.WriteString("\n]\n")
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	_, err = fmt.Fprintf(env.Stdout, "collected %d result(s) into %s\n", rows, env.Outputs[0])
	return err
}

// readTaskIndex reads catalog.TaskIndexName into names (task key -> name).
func readTaskIndex(p string, names map[string]string) error {
	data, err := readPart(p, catalog.TaskIndexName)
	if err != nil {
		return err
	}
	var index struct {
		Tasks []struct {
			Key  string `json:"key"`
			Name string `json:"name"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(data, &index); err != nil {
		return fmt.Errorf("%s: %w", catalog.TaskIndexName, err)
	}
	for _, t := range index.Tasks {
		names[t.Key] = t.Name
	}
	return nil
}

func readPart(p, name string) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxReportPart+1))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if len(data) > maxReportPart {
		return nil, fmt.Errorf("%s is over %d MiB, too big for a report", name, maxReportPart>>20)
	}
	return data, nil
}

// mdLine is s on one line: names come from operators and agents.
func mdLine(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}

// mdCell is s for a Markdown table cell.
func mdCell(s string) string { return strings.ReplaceAll(mdLine(s), "|", `\|`) }

// csvCell keeps a spreadsheet from reading a cell as a formula: model text
// and file names may start with = + - @ (a classic CSV injection).
func csvCell(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}
