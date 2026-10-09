package manager

import (
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
)

// Workflow conditions: the small, closed expression language a stage's
// "when" is written in. It is not a scripting language: there are no
// variables, no arithmetic, no function calls beyond the one .any(...)
// form below, and nothing in it can loop, allocate without bound or touch
// anything but the facts the manager already holds about earlier stages.
//
//	expr    = or
//	or      = and { "or" and }
//	and     = not { "and" not }
//	not     = "not" not | primary
//	primary = "(" expr ")" | "true" | "false"
//	        | stage "." ( "succeeded" | "failed" | "skipped" )
//	        | stage "." "state" ( "==" | "!=" ) STRING
//	        | count  ( "==" | "!=" | ">=" | "<=" | ">" | "<" ) NUMBER
//	        | files "." "any" "(" "name" "matches" STRING ")"
//	stage   = "previous" | "stage" "[" NUMBER "]"      (from 1; earlier stages only)
//	count   = stage "." "outputs" | "inputs"
//	files   = stage "." "outputs" | "inputs"
//
// STRING is double-quoted with no escapes except \" and \\; a state is
// COMPLETED, FAILED or SKIPPED; a pattern is a path.Match glob tried
// against each file's whole name and its last path element. Every
// expression is parsed, bounded and checked when the workflow is
// submitted, so evaluating one afterwards cannot fail.
const (
	maxConditionLen   = 512 // bytes
	maxConditionDepth = 8   // nested "(" and "not"
	maxConditionNodes = 64
	maxConditionGlob  = 128 // bytes
	maxConditionCount = 1_000_000
)

// ErrCondition is a "when" expression that isn't valid.
var ErrCondition = errors.New("manager: invalid condition")

// Stage states a condition can see (the stages before it have all been
// decided, so none is pending or running).
const (
	condCompleted = "COMPLETED"
	condFailed    = "FAILED"
	condSkipped   = "SKIPPED"
)

// CondStage is what a condition may know of one earlier stage.
type CondStage struct {
	State   string   // condCompleted, condFailed or condSkipped
	Outputs []string // the names of the files it handed on (none unless COMPLETED)
}

// CondEnv is what a condition is evaluated against: the workflow's own
// files and every stage before the one asking, in order.
type CondEnv struct {
	Inputs []string
	Stages []CondStage
}

type condKind int

const (
	condTrue condKind = iota
	condFalse
	condNot
	condAnd
	condOr
	condStateIs  // ref.state == / != str
	condCountCmp // ref.outputs / inputs  op  num
	condAnyMatch // ref.outputs / inputs .any(name matches str)
)

// condRef names the files or state of one source: stage -1 is the
// workflow's own files, otherwise the stage's index from 0 (previous is
// resolved to an index when parsed).
type condRef struct {
	stage    int
	previous bool // written as "previous" (kept for printing)
}

type condNode struct {
	kind  condKind
	a, b  *condNode
	ref   condRef
	field string // for condStateIs: "state" or a boolean field as "succeeded"/"failed"/"skipped"
	op    string // "==", "!=", ">=", ...
	num   int
	str   string
}

// Condition is a parsed, checked "when" expression.
type Condition struct {
	root *condNode
}

// ParseCondition parses src as the condition of the stage at index
// stage (from 0): it may refer only to the stages before it.
func ParseCondition(src string, stage int) (*Condition, error) {
	if len(src) > maxConditionLen {
		return nil, fmt.Errorf("%w: longer than %d bytes", ErrCondition, maxConditionLen)
	}
	for i := 0; i < len(src); i++ {
		if c := src[i]; c < 0x20 || c > 0x7e {
			return nil, fmt.Errorf("%w: only plain printable characters are allowed (byte %d)", ErrCondition, i+1)
		}
	}
	toks, err := lexCondition(src)
	if err != nil {
		return nil, err
	}
	p := &condParser{toks: toks, stage: stage}
	root, err := p.parseOr(0)
	if err != nil {
		return nil, err
	}
	if t := p.peek(); t.kind != tokEOF {
		return nil, p.errAt(t, "unexpected "+t.describe())
	}
	return &Condition{root: root}, nil
}

// String is the canonical spelling: parsing it again gives an equal
// condition.
func (c *Condition) String() string { return c.root.String() }

func (n *condNode) String() string { return n.render(0) }

// Binding strength, loosest first: a child gets parentheses only when it
// binds looser than where it stands, so printing never nests deeper than
// the expression it came from.
const (
	precOr = iota + 1
	precAnd
	precNot
	precAtom
)

func (n *condNode) render(need int) string {
	prec := precAtom
	var s string
	switch n.kind {
	case condNot:
		prec, s = precNot, "not "+n.a.render(precNot)
	case condAnd:
		prec, s = precAnd, n.a.render(precAnd)+" and "+n.b.render(precNot)
	case condOr:
		prec, s = precOr, n.a.render(precOr)+" or "+n.b.render(precAnd)
	default:
		s = n.atom()
	}
	if prec < need {
		return "(" + s + ")"
	}
	return s
}

func (n *condNode) atom() string {
	switch n.kind {
	case condTrue:
		return "true"
	case condFalse:
		return "false"
	case condStateIs:
		if n.field == "state" {
			return fmt.Sprintf("%s.state %s %s", n.ref.String(), n.op, strconv.Quote(n.str))
		}
		return n.ref.String() + "." + n.field
	case condCountCmp:
		return fmt.Sprintf("%s %s %d", n.ref.filesString(), n.op, n.num)
	case condAnyMatch:
		return fmt.Sprintf("%s.any(name matches %s)", n.ref.filesString(), strconv.Quote(n.str))
	}
	return "?"
}

func (r condRef) String() string {
	if r.previous {
		return "previous"
	}
	return fmt.Sprintf("stage[%d]", r.stage+1)
}

func (r condRef) filesString() string {
	if r.stage < 0 {
		return "inputs"
	}
	return r.String() + ".outputs"
}

// Eval reports whether the condition holds. env must cover every stage
// the condition refers to (ParseCondition guarantees they are earlier
// than the stage that carries it).
func (c *Condition) Eval(env CondEnv) bool { return c.root.eval(env) }

func (n *condNode) eval(env CondEnv) bool {
	switch n.kind {
	case condTrue:
		return true
	case condFalse:
		return false
	case condNot:
		return !n.a.eval(env)
	case condAnd:
		return n.a.eval(env) && n.b.eval(env)
	case condOr:
		return n.a.eval(env) || n.b.eval(env)
	case condStateIs:
		st := n.ref.state(env)
		switch n.field {
		case "succeeded":
			return st == condCompleted
		case "failed":
			return st == condFailed
		case "skipped":
			return st == condSkipped
		}
		if n.op == "==" {
			return st == n.str
		}
		return st != n.str
	case condCountCmp:
		have := len(n.ref.files(env))
		switch n.op {
		case "==":
			return have == n.num
		case "!=":
			return have != n.num
		case ">=":
			return have >= n.num
		case "<=":
			return have <= n.num
		case ">":
			return have > n.num
		case "<":
			return have < n.num
		}
	case condAnyMatch:
		for _, name := range n.ref.files(env) {
			if globMatches(n.str, name) {
				return true
			}
		}
	}
	return false
}

func (r condRef) state(env CondEnv) string {
	if r.stage < 0 || r.stage >= len(env.Stages) {
		return condSkipped // can't happen for a parsed condition
	}
	return env.Stages[r.stage].State
}

func (r condRef) files(env CondEnv) []string {
	if r.stage < 0 {
		return env.Inputs
	}
	if r.stage >= len(env.Stages) {
		return nil
	}
	return env.Stages[r.stage].Outputs
}

// globMatches: pattern against the whole name, or its last element.
func globMatches(pattern, name string) bool {
	if ok, _ := path.Match(pattern, name); ok {
		return true
	}
	ok, _ := path.Match(pattern, path.Base(name))
	return ok
}

// ---- lexer

type tokKind int

const (
	tokEOF tokKind = iota
	tokIdent
	tokNumber
	tokString
	tokPunct // ( ) [ ] . == != >= <= > <
)

type condToken struct {
	kind tokKind
	text string
	pos  int // byte offset, from 0
}

func (t condToken) describe() string {
	switch t.kind {
	case tokEOF:
		return "end of the condition"
	case tokString:
		return "text " + strconv.Quote(t.text)
	}
	return strconv.Quote(t.text)
}

func lexCondition(src string) ([]condToken, error) {
	var toks []condToken
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == ' ' || c == '\t':
			i++
		case c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_':
			j := i
			for j < len(src) && (src[j] >= 'a' && src[j] <= 'z' || src[j] >= 'A' && src[j] <= 'Z' || src[j] == '_' || src[j] >= '0' && src[j] <= '9') {
				j++
			}
			toks = append(toks, condToken{tokIdent, src[i:j], i})
			i = j
		case c >= '0' && c <= '9':
			j := i
			for j < len(src) && src[j] >= '0' && src[j] <= '9' {
				j++
			}
			toks = append(toks, condToken{tokNumber, src[i:j], i})
			i = j
		case c == '"':
			var b strings.Builder
			j := i + 1
			for ; j < len(src) && src[j] != '"'; j++ {
				if src[j] == '\\' {
					j++
					if j >= len(src) || (src[j] != '"' && src[j] != '\\') {
						return nil, fmt.Errorf("%w: at byte %d: only \\\" and \\\\ may follow a backslash", ErrCondition, j)
					}
				}
				b.WriteByte(src[j])
			}
			if j >= len(src) {
				return nil, fmt.Errorf("%w: at byte %d: text starting here is never closed", ErrCondition, i+1)
			}
			toks = append(toks, condToken{tokString, b.String(), i})
			i = j + 1
		case strings.ContainsRune("()[].", rune(c)):
			toks = append(toks, condToken{tokPunct, string(c), i})
			i++
		case c == '=' || c == '!' || c == '>' || c == '<':
			if i+1 < len(src) && src[i+1] == '=' {
				toks = append(toks, condToken{tokPunct, src[i : i+2], i})
				i += 2
			} else if c == '>' || c == '<' {
				toks = append(toks, condToken{tokPunct, string(c), i})
				i++
			} else {
				return nil, fmt.Errorf("%w: at byte %d: %q on its own isn't an operator (use ==, !=, >=, <=, > or <)", ErrCondition, i+1, string(c))
			}
		default:
			return nil, fmt.Errorf("%w: at byte %d: unexpected %q", ErrCondition, i+1, string(c))
		}
		if len(toks) > 4*maxConditionNodes {
			return nil, fmt.Errorf("%w: too long", ErrCondition)
		}
	}
	return append(toks, condToken{tokEOF, "", len(src)}), nil
}

// ---- parser

type condParser struct {
	toks  []condToken
	i     int
	stage int // index of the stage that carries the condition
	nodes int
}

func (p *condParser) peek() condToken { return p.toks[p.i] }
func (p *condParser) next() condToken {
	t := p.toks[p.i]
	if t.kind != tokEOF {
		p.i++
	}
	return t
}

func (p *condParser) errAt(t condToken, msg string) error {
	return fmt.Errorf("%w: at byte %d: %s", ErrCondition, t.pos+1, msg)
}

func (p *condParser) node(n condNode) (*condNode, error) {
	if p.nodes++; p.nodes > maxConditionNodes {
		return nil, fmt.Errorf("%w: more than %d parts", ErrCondition, maxConditionNodes)
	}
	return &n, nil
}

func (p *condParser) isWord(w string) bool {
	t := p.peek()
	return t.kind == tokIdent && t.text == w
}

func (p *condParser) isPunct(s string) bool {
	t := p.peek()
	return t.kind == tokPunct && t.text == s
}

func (p *condParser) expectPunct(s string) error {
	if !p.isPunct(s) {
		t := p.peek()
		return p.errAt(t, fmt.Sprintf("expected %q, found %s", s, t.describe()))
	}
	p.next()
	return nil
}

func (p *condParser) expectWord(w string) error {
	if !p.isWord(w) {
		t := p.peek()
		return p.errAt(t, fmt.Sprintf("expected %q, found %s", w, t.describe()))
	}
	p.next()
	return nil
}

func (p *condParser) parseOr(depth int) (*condNode, error) {
	left, err := p.parseAnd(depth)
	if err != nil {
		return nil, err
	}
	for p.isWord("or") {
		p.next()
		right, err := p.parseAnd(depth)
		if err != nil {
			return nil, err
		}
		if left, err = p.node(condNode{kind: condOr, a: left, b: right}); err != nil {
			return nil, err
		}
	}
	return left, nil
}

func (p *condParser) parseAnd(depth int) (*condNode, error) {
	left, err := p.parseNot(depth)
	if err != nil {
		return nil, err
	}
	for p.isWord("and") {
		p.next()
		right, err := p.parseNot(depth)
		if err != nil {
			return nil, err
		}
		if left, err = p.node(condNode{kind: condAnd, a: left, b: right}); err != nil {
			return nil, err
		}
	}
	return left, nil
}

func (p *condParser) parseNot(depth int) (*condNode, error) {
	if p.isWord("not") {
		if depth+1 > maxConditionDepth {
			return nil, p.errAt(p.peek(), fmt.Sprintf("nested more than %d deep", maxConditionDepth))
		}
		p.next()
		inner, err := p.parseNot(depth + 1)
		if err != nil {
			return nil, err
		}
		return p.node(condNode{kind: condNot, a: inner})
	}
	return p.parsePrimary(depth)
}

func (p *condParser) parsePrimary(depth int) (*condNode, error) {
	t := p.peek()
	switch {
	case p.isPunct("("):
		if depth+1 > maxConditionDepth {
			return nil, p.errAt(t, fmt.Sprintf("nested more than %d deep", maxConditionDepth))
		}
		p.next()
		inner, err := p.parseOr(depth + 1)
		if err != nil {
			return nil, err
		}
		if err := p.expectPunct(")"); err != nil {
			return nil, err
		}
		return inner, nil
	case p.isWord("true"):
		p.next()
		return p.node(condNode{kind: condTrue})
	case p.isWord("false"):
		p.next()
		return p.node(condNode{kind: condFalse})
	case p.isWord("inputs"):
		p.next()
		return p.parseFiles(condRef{stage: -1}, true)
	case p.isWord("previous"), p.isWord("stage"):
		ref, err := p.parseStageRef()
		if err != nil {
			return nil, err
		}
		if err := p.expectPunct("."); err != nil {
			return nil, err
		}
		f := p.next()
		if f.kind != tokIdent {
			return nil, p.errAt(f, "expected succeeded, failed, skipped, state or outputs after the dot, found "+f.describe())
		}
		switch f.text {
		case "succeeded", "failed", "skipped":
			return p.node(condNode{kind: condStateIs, ref: ref, field: f.text})
		case "state":
			op := p.next()
			if op.kind != tokPunct || (op.text != "==" && op.text != "!=") {
				return nil, p.errAt(op, "a state is compared with == or !=")
			}
			s := p.next()
			if s.kind != tokString {
				return nil, p.errAt(s, `compare the state with a quoted name, e.g. "COMPLETED"`)
			}
			if s.text != condCompleted && s.text != condFailed && s.text != condSkipped {
				return nil, p.errAt(s, fmt.Sprintf("%q isn't a state a stage can be in here (COMPLETED, FAILED or SKIPPED)", s.text))
			}
			return p.node(condNode{kind: condStateIs, ref: ref, field: "state", op: op.text, str: s.text})
		case "outputs":
			return p.parseFiles(ref, false)
		}
		return nil, p.errAt(f, fmt.Sprintf("a stage has no %q (succeeded, failed, skipped, state or outputs)", f.text))
	}
	return nil, p.errAt(t, "expected a condition such as previous.succeeded, found "+t.describe())
}

// parseStageRef reads "previous" or "stage[K]" and resolves it to a
// stage index, refusing the stage's own and later ones.
func (p *condParser) parseStageRef() (condRef, error) {
	t := p.next()
	if t.text == "previous" {
		if p.stage < 1 {
			return condRef{}, p.errAt(t, "step 1 has no previous step")
		}
		return condRef{stage: p.stage - 1, previous: true}, nil
	}
	if err := p.expectPunct("["); err != nil {
		return condRef{}, err
	}
	n := p.next()
	if n.kind != tokNumber || len(n.text) > 3 {
		return condRef{}, p.errAt(n, "expected a step number, e.g. stage[1]")
	}
	k, _ := strconv.Atoi(n.text)
	if k < 1 || k > p.stage {
		return condRef{}, p.errAt(n, fmt.Sprintf("step %d can only look at steps 1-%d, the ones before it", p.stage+1, p.stage))
	}
	if err := p.expectPunct("]"); err != nil {
		return condRef{}, err
	}
	return condRef{stage: k - 1}, nil
}

// parseFiles reads what may follow a file list (a stage's outputs, or
// inputs): a comparison of its length, or .any(name matches "glob").
func (p *condParser) parseFiles(ref condRef, bare bool) (*condNode, error) {
	if p.isPunct(".") {
		p.next()
		if err := p.expectWord("any"); err != nil {
			return nil, err
		}
		for _, step := range []func() error{
			func() error { return p.expectPunct("(") },
			func() error { return p.expectWord("name") },
			func() error { return p.expectWord("matches") },
		} {
			if err := step(); err != nil {
				return nil, err
			}
		}
		s := p.next()
		if s.kind != tokString {
			return nil, p.errAt(s, `expected a quoted pattern, e.g. "*.png"`)
		}
		if len(s.text) == 0 || len(s.text) > maxConditionGlob {
			return nil, p.errAt(s, fmt.Sprintf("a pattern is 1-%d bytes", maxConditionGlob))
		}
		if _, err := path.Match(s.text, ""); err != nil {
			return nil, p.errAt(s, fmt.Sprintf("bad pattern %q: %v", s.text, err))
		}
		if err := p.expectPunct(")"); err != nil {
			return nil, err
		}
		return p.node(condNode{kind: condAnyMatch, ref: ref, str: s.text})
	}
	op := p.next()
	if op.kind != tokPunct || !strings.Contains(" == != >= <= > < ", " "+op.text+" ") {
		what := "previous.outputs"
		if bare {
			what = "inputs"
		}
		return nil, p.errAt(op, fmt.Sprintf("compare %s with a number (e.g. %s >= 1) or use .any(name matches \"*.png\")", what, what))
	}
	n := p.next()
	if n.kind != tokNumber || len(n.text) > 7 {
		return nil, p.errAt(n, "expected a whole number")
	}
	v, _ := strconv.Atoi(n.text)
	if v > maxConditionCount {
		return nil, p.errAt(n, fmt.Sprintf("numbers go up to %d", maxConditionCount))
	}
	return p.node(condNode{kind: condCountCmp, ref: ref, op: op.text, num: v})
}
