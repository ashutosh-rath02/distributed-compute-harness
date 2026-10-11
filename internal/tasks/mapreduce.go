package tasks

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
)

// The map-reduce pair: text.split cuts a text file into chunks (the
// fan-out), json.merge folds the chunks' JSON results back into one (the
// fan-in). Both are deterministic: the same input and parameters give
// byte-identical output on every device, so spot checks apply.

// ---- text.split

type textSplit struct{}

func (textSplit) Available(ctx context.Context) error { return always(ctx) }

// Run writes exactly `chunks` files, cutting only after a newline, so the
// chunks joined in order are the input byte for byte (and the lines,
// words and bytes counted per chunk add up to the whole file's).
func (textSplit) Run(ctx context.Context, env Env) error {
	n, err := strconv.Atoi(env.Params["chunks"])
	if err != nil || n < 1 || n != len(env.Outputs) {
		return fmt.Errorf("text.split: %d chunks asked for, %d outputs declared", n, len(env.Outputs))
	}
	src := env.in(0)
	var lines, size int64
	if env.Params["by"] == "bytes" {
		st, err := os.Stat(src)
		if err != nil {
			return err
		}
		size = st.Size()
	} else {
		var err error
		if lines, err = countLines(ctx, src); err != nil {
			return err
		}
	}
	files := make([]*os.File, n)
	writers := make([]*bufio.Writer, n)
	defer func() {
		for _, f := range files {
			if f != nil {
				f.Close()
			}
		}
	}()
	for k := range files {
		f, err := os.Create(env.out(k))
		if err != nil {
			return err
		}
		files[k], writers[k] = f, bufio.NewWriter(f)
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	perChunkLines := (lines + int64(n) - 1) / int64(n)
	perChunkBytes := (size + int64(n) - 1) / int64(n)
	var idx int
	var lineNo, pos int64
	r := bufio.NewReaderSize(in, 64<<10)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		frag, err := r.ReadSlice('\n')
		if len(frag) > 0 {
			if _, werr := writers[idx].Write(frag); werr != nil {
				return werr
			}
			pos += int64(len(frag))
		}
		atLineEnd := len(frag) > 0 && frag[len(frag)-1] == '\n'
		if atLineEnd || errors.Is(err, io.EOF) {
			if len(frag) > 0 {
				lineNo++
			}
			if env.Params["by"] == "bytes" {
				for idx < n-1 && pos >= perChunkBytes*int64(idx+1) {
					idx++
				}
			} else if perChunkLines > 0 && lineNo%perChunkLines == 0 && idx < n-1 {
				idx++
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue // a very long line: keep going in the same chunk
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return err
		}
	}
	for k, w := range writers {
		if err := w.Flush(); err != nil {
			return err
		}
		if err := files[k].Close(); err != nil {
			return err
		}
		files[k] = nil
	}
	_, err = fmt.Fprintf(env.Stdout, "%d bytes cut into %d chunk(s)\n", pos, n)
	return err
}

// countLines counts lines the way the split cuts them: a final line
// without a newline is a line too.
func countLines(ctx context.Context, p string) (int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	buf := make([]byte, 64<<10)
	var lines int64
	var last byte = '\n'
	for {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		k, err := f.Read(buf)
		if k > 0 {
			lines += int64(bytes.Count(buf[:k], []byte{'\n'}))
			last = buf[k-1]
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, err
		}
	}
	if last != '\n' {
		lines++
	}
	return lines, nil
}

// ---- json.merge

type jsonMerge struct{}

const (
	mergeMaxFileBytes  = 8 << 20  // one input
	mergeMaxTotalBytes = 64 << 20 // all inputs together
	mergeMaxDepth      = 32
)

func (jsonMerge) Available(ctx context.Context) error { return always(ctx) }
func (jsonMerge) Run(ctx context.Context, env Env) error {
	order := make([]int, len(env.Inputs))
	for i := range order {
		order[i] = i
	}
	// Name order, not submission order: the result must not depend on how
	// the manager happened to list the parts.
	sort.SliceStable(order, func(a, b int) bool { return env.Inputs[order[a]] < env.Inputs[order[b]] })
	mode, field := env.Params["mode"], env.Params["field"]
	var total int64
	var docs []any
	names := make([]string, 0, len(order))
	for _, i := range order {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		name := env.Inputs[i]
		raw, err := readLimited(env.in(i), mergeMaxFileBytes)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if total += int64(len(raw)); total > mergeMaxTotalBytes {
			return fmt.Errorf("the inputs together are over %d MiB", mergeMaxTotalBytes>>20)
		}
		doc, err := decodeJSON(raw)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if field != "" {
			for _, key := range strings.Split(field, ".") {
				obj, ok := doc.(map[string]any)
				if !ok {
					return fmt.Errorf("%s: no field %q (not an object at %q)", name, field, key)
				}
				if doc, ok = obj[key]; !ok {
					return fmt.Errorf("%s: no field %q", name, field)
				}
			}
		}
		docs = append(docs, doc)
		names = append(names, name)
	}
	var result any
	switch mode {
	case "concat":
		all := []any{}
		for k, d := range docs {
			arr, ok := d.([]any)
			if !ok {
				return fmt.Errorf("%s: concat needs a JSON array, got %s", names[k], jsonKind(d))
			}
			all = append(all, arr...)
		}
		result = all
	case "collect":
		obj := make(map[string]any, len(docs))
		for k, d := range docs {
			obj[names[k]] = d
		}
		result = obj
	case "sum":
		acc := docs[0]
		for k := 1; k < len(docs); k++ {
			var err error
			if acc, err = sumJSON(acc, docs[k], ""); err != nil {
				return fmt.Errorf("%s: %w", names[k], err)
			}
		}
		// A single input still has to be summable (numbers, objects, arrays).
		if len(docs) == 1 {
			if _, err := sumJSON(acc, acc, ""); err != nil {
				return fmt.Errorf("%s: %w", names[0], err)
			}
		}
		result = acc
	default:
		return fmt.Errorf("unknown mode %q", mode)
	}
	out, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(env.out(0), append(out, '\n'), 0o600); err != nil {
		return err
	}
	_, err = fmt.Fprintf(env.Stdout, "merged %d file(s) (%s)\n", len(docs), mode)
	return err
}

func readLimited(p string, limit int64) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("over %d MiB", limit>>20)
	}
	return b, nil
}

// decodeJSON reads one JSON value, keeping numbers exact (json.Number),
// refusing trailing data and anything nested deeper than mergeMaxDepth.
func decodeJSON(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("not JSON: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("not JSON: more than one value")
	}
	if depth(v) > mergeMaxDepth {
		return nil, fmt.Errorf("nested deeper than %d", mergeMaxDepth)
	}
	return v, nil
}

func depth(v any) int {
	d := 0
	switch x := v.(type) {
	case map[string]any:
		for _, e := range x {
			d = max(d, depth(e))
		}
		return d + 1
	case []any:
		for _, e := range x {
			d = max(d, depth(e))
		}
		return d + 1
	}
	return 0
}

func jsonKind(v any) string {
	switch v.(type) {
	case map[string]any:
		return "an object"
	case []any:
		return "an array"
	case json.Number:
		return "a number"
	case string:
		return "text"
	case bool:
		return "true/false"
	}
	return "null"
}

// sumJSON adds b into a: numbers add (exact for whole numbers, with an
// overflow check), objects merge key by key, arrays join. Anything else,
// or two different kinds, is an error naming the path.
func sumJSON(a, b any, at string) (any, error) {
	where := at
	if where == "" {
		where = "the top level"
	}
	switch x := a.(type) {
	case json.Number:
		y, ok := b.(json.Number)
		if !ok {
			return nil, fmt.Errorf("at %s: can't add %s to a number", where, jsonKind(b))
		}
		return addNumbers(x, y, where)
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("at %s: can't add %s to an object", where, jsonKind(b))
		}
		out := make(map[string]any, len(x)+len(y))
		for k, v := range x {
			out[k] = v
		}
		for k, v := range y {
			if have, ok := out[k]; ok {
				sum, err := sumJSON(have, v, at+"/"+k)
				if err != nil {
					return nil, err
				}
				out[k] = sum
			} else {
				out[k] = v
			}
		}
		return out, nil
	case []any:
		y, ok := b.([]any)
		if !ok {
			return nil, fmt.Errorf("at %s: can't join %s to an array", where, jsonKind(b))
		}
		return append(append([]any{}, x...), y...), nil
	}
	return nil, fmt.Errorf("at %s: can't add %s (sum adds numbers, merges objects and joins arrays)", where, jsonKind(a))
}

func addNumbers(x, y json.Number, where string) (any, error) {
	xi, errX := strconv.ParseInt(string(x), 10, 64)
	yi, errY := strconv.ParseInt(string(y), 10, 64)
	if errX == nil && errY == nil {
		s := xi + yi
		if (yi > 0 && s < xi) || (yi < 0 && s > xi) {
			return nil, fmt.Errorf("at %s: the sum overflows", where)
		}
		return json.Number(strconv.FormatInt(s, 10)), nil
	}
	xf, errX := strconv.ParseFloat(string(x), 64)
	yf, errY := strconv.ParseFloat(string(y), 64)
	if errX != nil || errY != nil {
		return nil, fmt.Errorf("at %s: not a usable number", where)
	}
	s := xf + yf
	if math.IsInf(s, 0) || math.IsNaN(s) {
		return nil, fmt.Errorf("at %s: the sum overflows", where)
	}
	return json.Number(strconv.FormatFloat(s, 'g', -1, 64)), nil
}
