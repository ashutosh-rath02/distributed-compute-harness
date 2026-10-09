package manager

import (
	"fmt"
	"strconv"
	"strings"
)

// parseYAMLSubset reads the small, strict subset of YAML that
// docs/openapi.yaml is written in, so the spec can be checked without a YAML
// dependency. Supported: block mappings and sequences (indent by spaces),
// "- key: value" items, plain / "double" / 'single' quoted scalars, literal
// block scalars ("|"), "[]" "{}" and one-line "[a, b]" lists of scalars,
// comments on their own line. Anything else is an error, so the spec stays
// in a form every real YAML parser also reads the same way: plain scalars
// that start with an indicator character or contain ": " or " #" are
// rejected rather than guessed at.
func parseYAMLSubset(src string) (any, error) {
	p := &yamlParser{lines: strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")}
	for i, l := range p.lines {
		if strings.Contains(l, "\t") {
			return nil, fmt.Errorf("line %d: tabs are not allowed", i+1)
		}
		p.lines[i] = strings.TrimRight(l, " ")
	}
	v, err := p.node(0)
	if err != nil {
		return nil, err
	}
	p.skip()
	if p.pos < len(p.lines) {
		return nil, p.errf("unexpected content (bad indentation?)")
	}
	return v, nil
}

type yamlParser struct {
	lines []string
	pos   int
}

func (p *yamlParser) errf(format string, a ...any) error {
	return fmt.Errorf("line %d: %s", p.pos+1, fmt.Sprintf(format, a...))
}

func indentOf(l string) int { return len(l) - len(strings.TrimLeft(l, " ")) }

// skip moves past blank lines and comments.
func (p *yamlParser) skip() {
	for p.pos < len(p.lines) {
		t := strings.TrimSpace(p.lines[p.pos])
		if t == "" || strings.HasPrefix(t, "#") {
			p.pos++
			continue
		}
		return
	}
}

func isSeqItem(t string) bool { return t == "-" || strings.HasPrefix(t, "- ") }

// node parses the block starting at the next content line, which must be
// indented at least min.
func (p *yamlParser) node(min int) (any, error) {
	p.skip()
	if p.pos >= len(p.lines) {
		return nil, nil
	}
	ind := indentOf(p.lines[p.pos])
	if ind < min {
		return nil, nil
	}
	if isSeqItem(strings.TrimSpace(p.lines[p.pos])) {
		return p.seq(ind)
	}
	return p.mapping(ind)
}

func (p *yamlParser) mapping(ind int) (any, error) {
	out := map[string]any{}
	for {
		p.skip()
		if p.pos >= len(p.lines) {
			return out, nil
		}
		line := p.lines[p.pos]
		li := indentOf(line)
		if li < ind {
			return out, nil
		}
		if li > ind {
			return nil, p.errf("unexpected indentation")
		}
		text := strings.TrimSpace(line)
		if isSeqItem(text) {
			return nil, p.errf("a list item where a key was expected (indent list items under their key)")
		}
		key, rest, ok := splitYAMLKey(text)
		if !ok {
			return nil, p.errf("expected \"key: value\", got %q", text)
		}
		if _, dup := out[key]; dup {
			return nil, p.errf("duplicate key %q", key)
		}
		keyLine := p.pos
		p.pos++
		switch {
		case rest == "":
			p.skip()
			if p.pos < len(p.lines) && indentOf(p.lines[p.pos]) > ind {
				v, err := p.node(ind + 1)
				if err != nil {
					return nil, err
				}
				out[key] = v
			} else {
				out[key] = nil
			}
		case rest == "|" || rest == "|-":
			out[key] = p.block(ind, rest == "|-")
		default:
			v, err := p.scalar(rest, keyLine)
			if err != nil {
				return nil, err
			}
			out[key] = v
		}
	}
}

func (p *yamlParser) seq(ind int) (any, error) {
	out := []any{}
	for {
		p.skip()
		if p.pos >= len(p.lines) {
			return out, nil
		}
		line := p.lines[p.pos]
		li := indentOf(line)
		text := strings.TrimSpace(line)
		if li < ind || (li == ind && !isSeqItem(text)) {
			return out, nil
		}
		if li > ind {
			return nil, p.errf("unexpected indentation in list")
		}
		content := strings.TrimSpace(strings.TrimPrefix(text, "-"))
		switch {
		case content == "":
			p.pos++
			v, err := p.node(ind + 1)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		default:
			if _, _, isKey := splitYAMLKey(content); isKey {
				// "- key: value" starts a mapping whose keys line up after the dash.
				p.lines[p.pos] = strings.Repeat(" ", ind+2) + content
				v, err := p.mapping(ind + 2)
				if err != nil {
					return nil, err
				}
				out = append(out, v)
				continue
			}
			lineNo := p.pos
			p.pos++
			v, err := p.scalar(content, lineNo)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
	}
}

// block reads a literal block scalar whose lines are indented more than the key.
func (p *yamlParser) block(keyIndent int, strip bool) string {
	var raw []string
	for p.pos < len(p.lines) {
		l := p.lines[p.pos]
		if strings.TrimSpace(l) != "" && indentOf(l) <= keyIndent {
			break
		}
		raw = append(raw, l)
		p.pos++
	}
	for len(raw) > 0 && strings.TrimSpace(raw[len(raw)-1]) == "" {
		raw = raw[:len(raw)-1]
	}
	min := -1
	for _, l := range raw {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if i := indentOf(l); min < 0 || i < min {
			min = i
		}
	}
	for i, l := range raw {
		if len(l) >= min && min > 0 {
			raw[i] = l[min:]
		} else {
			raw[i] = strings.TrimSpace(l)
		}
	}
	s := strings.Join(raw, "\n")
	if !strip {
		s += "\n"
	}
	return s
}

// splitYAMLKey splits "key: rest". The key may be plain or "double quoted".
func splitYAMLKey(text string) (key, rest string, ok bool) {
	if strings.HasPrefix(text, `"`) {
		end := closingQuote(text)
		if end < 0 || end+1 >= len(text) || text[end+1] != ':' {
			return "", "", false
		}
		k, err := strconv.Unquote(text[:end+1])
		if err != nil {
			return "", "", false
		}
		after := text[end+2:]
		if after != "" && after[0] != ' ' {
			return "", "", false
		}
		return k, strings.TrimSpace(after), true
	}
	if strings.HasPrefix(text, "'") || strings.HasPrefix(text, "[") || strings.HasPrefix(text, "{") {
		return "", "", false
	}
	if strings.HasSuffix(text, ":") {
		return text[:len(text)-1], "", !strings.Contains(text[:len(text)-1], ": ")
	}
	i := strings.Index(text, ": ")
	if i <= 0 {
		return "", "", false
	}
	return text[:i], strings.TrimSpace(text[i+2:]), true
}

// closingQuote finds the index of the closing " of a string starting at 0.
func closingQuote(s string) int {
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			return i
		}
	}
	return -1
}

func (p *yamlParser) scalar(s string, lineNo int) (any, error) {
	fail := func(format string, a ...any) (any, error) {
		return nil, fmt.Errorf("line %d: %s", lineNo+1, fmt.Sprintf(format, a...))
	}
	switch {
	case s == "[]":
		return []any{}, nil
	case s == "{}":
		return map[string]any{}, nil
	case strings.HasPrefix(s, "["):
		if !strings.HasSuffix(s, "]") {
			return fail("unterminated [ list")
		}
		out := []any{}
		for _, part := range strings.Split(s[1:len(s)-1], ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				return fail("empty item in [ list ]")
			}
			v, err := p.scalar(part, lineNo)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case strings.HasPrefix(s, `"`):
		if closingQuote(s) != len(s)-1 {
			return fail("text after the closing quote")
		}
		v, err := strconv.Unquote(s)
		if err != nil {
			return fail("bad double-quoted string: %v", err)
		}
		return v, nil
	case strings.HasPrefix(s, "'"):
		if len(s) < 2 || !strings.HasSuffix(s, "'") {
			return fail("unterminated single-quoted string")
		}
		inner := s[1 : len(s)-1]
		if strings.Contains(strings.ReplaceAll(inner, "''", ""), "'") {
			return fail("a ' inside a single-quoted string must be doubled")
		}
		return strings.ReplaceAll(inner, "''", "'"), nil
	}
	if strings.ContainsRune("`@%&*!|>{}?,", rune(s[0])) {
		return fail("a plain value may not start with %q: quote it", s[:1])
	}
	if strings.Contains(s, ": ") || strings.Contains(s, " #") || strings.HasSuffix(s, ":") {
		return fail("a plain value may not contain \": \" or \" #\" (or end in a colon): quote it")
	}
	switch s {
	case "true":
		return true, nil
	case "false":
		return false, nil
	case "null", "~":
		return nil, nil
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return int(n), nil
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f, nil
	}
	return s, nil
}
