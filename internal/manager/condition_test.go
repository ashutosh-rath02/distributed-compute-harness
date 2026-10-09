package manager

import (
	"strings"
	"testing"
)

func testEnv() CondEnv {
	return CondEnv{
		Inputs: []string{"a.txt", "b.png"},
		Stages: []CondStage{
			{State: condCompleted, Outputs: []string{"x.jpg", "parts/0001/y.png", "z.png"}},
			{State: condFailed},
			{State: condSkipped},
		},
	}
}

func TestConditionEval(t *testing.T) {
	cases := []struct {
		src  string
		want bool
	}{
		{"previous.skipped", true},
		{"previous.succeeded", false},
		{"stage[2].failed", true},
		{"stage[1].succeeded and stage[2].failed", true},
		{`stage[1].state == "COMPLETED"`, true},
		{`stage[3].state != "SKIPPED"`, false},
		{"stage[1].outputs >= 3", true},
		{"stage[1].outputs > 3", false},
		{"stage[2].outputs == 0", true},
		{`stage[1].outputs.any(name matches "*.png")`, true},
		{`stage[1].outputs.any(name matches "y.*")`, true}, // the last element of parts/0001/y.png
		{`stage[1].outputs.any(name matches "*.gif")`, false},
		{`inputs.any(name matches "*.png")`, true},
		{"inputs == 2", true},
		{"not previous.succeeded", true},
		{"not (previous.skipped and false)", true},
		{"false or true and false", false}, // and binds tighter than or
		{"(false or true) and false", false},
		{"true or false and false", true},
	}
	for _, c := range cases {
		cond, err := ParseCondition(c.src, 3)
		if err != nil {
			t.Fatalf("%q: %v", c.src, err)
		}
		if got := cond.Eval(testEnv()); got != c.want {
			t.Errorf("%q = %v, want %v", c.src, got, c.want)
		}
	}
}

func TestConditionRefused(t *testing.T) {
	cases := map[string]struct {
		stage int
		src   string
		want  string
	}{
		"first step has no previous": {0, "previous.succeeded", "no previous"},
		"itself":                     {2, "stage[3].succeeded", "can only look at steps 1-2"},
		"later":                      {1, "stage[4].succeeded", "can only look at steps 1-1"},
		"zero":                       {2, "stage[0].succeeded", "can only look"},
		"bare outputs":               {2, "previous.outputs", "compare previous.outputs"},
		"unknown field":              {2, "previous.colour", `no "colour"`},
		"unknown state":              {2, `previous.state == "RUNNING"`, "isn't a state"},
		"state needs text":           {2, "previous.state == 1", "quoted name"},
		"state needs ==":             {2, `previous.state >= "FAILED"`, "== or !="},
		"bad glob":                   {2, `previous.outputs.any(name matches "[a")`, "bad pattern"},
		"empty glob":                 {2, `previous.outputs.any(name matches "")`, "pattern is"},
		"assignment":                 {2, "previous.outputs = 1", "isn't an operator"},
		"unterminated":               {2, `previous.outputs.any(name matches "abc`, "never closed"},
		"bad escape":                 {2, `previous.outputs.any(name matches "a\n")`, "backslash"},
		"trailing":                   {2, "true false", "unexpected"},
		"unknown word":               {2, "launch()", "expected a condition"},
		"huge number":                {2, "previous.outputs >= 99999999", "whole number"},
		"big number":                 {2, "previous.outputs >= 2000000", "numbers go up to"},
		"empty":                      {2, "", "expected a condition"},
		"control char":               {2, "true\x00", "printable"},
		"unclosed paren":             {2, "(true", `expected ")"`},
	}
	for name, c := range cases {
		_, err := ParseCondition(c.src, c.stage)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %q -> %v, want error containing %q", name, c.src, err, c.want)
		}
	}
}

func TestConditionBounds(t *testing.T) {
	deep := strings.Repeat("(", 9) + "true" + strings.Repeat(")", 9)
	if _, err := ParseCondition(deep, 1); err == nil || !strings.Contains(err.Error(), "nested") {
		t.Errorf("9 deep: %v", err)
	}
	if _, err := ParseCondition(strings.Repeat("(", 8)+"true"+strings.Repeat(")", 8), 1); err != nil {
		t.Errorf("8 deep: %v", err)
	}
	nots := strings.Repeat("not ", 9) + "true"
	if _, err := ParseCondition(nots, 1); err == nil {
		t.Error("9 nots accepted")
	}
	long := strings.Repeat("true or ", 70) + "true"
	if _, err := ParseCondition(long, 1); err == nil {
		t.Error("over 64 parts accepted")
	}
	if _, err := ParseCondition(strings.Repeat("x", maxConditionLen+1), 1); err == nil {
		t.Error("over the length limit accepted")
	}
}

func TestConditionRoundTrip(t *testing.T) {
	for _, src := range []string{
		"a", "true or false or true", "true and (false or true)", "not not previous.failed",
		`not (previous.succeeded and stage[1].outputs.any(name matches "*.p\"ng"))`,
		"true or (false or true)", "(true and false) or true",
	} {
		if src == "a" {
			continue
		}
		c, err := ParseCondition(src, 3)
		if err != nil {
			t.Fatalf("%q: %v", src, err)
		}
		c2, err := ParseCondition(c.String(), 3)
		if err != nil {
			t.Fatalf("%q printed as %q: %v", src, c.String(), err)
		}
		if c2.String() != c.String() {
			t.Errorf("%q: %q != %q", src, c.String(), c2.String())
		}
	}
}

func FuzzCondition(f *testing.F) {
	for _, s := range []string{
		"previous.succeeded", "previous.failed or stage[1].outputs >= 2",
		`inputs.any(name matches "*.png") and not previous.skipped`,
		`stage[2].state == "COMPLETED"`, "((((true))))", "not not not true", `"`, "stage[", "\\", "",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		c, err := ParseCondition(src, 8)
		if err != nil {
			return
		}
		if len(src) > maxConditionLen {
			t.Fatalf("accepted %d bytes", len(src))
		}
		again, err := ParseCondition(c.String(), 8)
		if err != nil {
			t.Fatalf("%q printed as %q, which doesn't parse: %v", src, c.String(), err)
		}
		if again.String() != c.String() {
			t.Fatalf("%q: unstable printing %q vs %q", src, c.String(), again.String())
		}
		env := CondEnv{Inputs: []string{"a.png"}}
		for i := 0; i < 8; i++ {
			env.Stages = append(env.Stages, CondStage{State: condCompleted, Outputs: []string{"x/y.png"}})
		}
		if c.Eval(env) != again.Eval(env) {
			t.Fatalf("%q: evaluates differently after printing", src)
		}
	})
}
