package main

import (
	"encoding/json"
	"flag"
	"io"
	"strings"
	"testing"
)

func TestWorkflowStepFlagsKeepTheirOrderAndCombine(t *testing.T) {
	fs := flag.NewFlagSet("workflow", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var steps stepFlags
	fs.Var(stepFlag{&steps}, "step", "")
	fs.Var(combineFlag{&steps}, "combine", "")
	err := fs.Parse([]string{
		"-step", "render.fractal parts=12 scene=spiral",
		"-combine", "image.stack name=whole.png",
		"-step", "image.resize each width=800",
		"-step", "file.hash",
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(steps.stages)
	want := `[{"combine":{"params":{"name":"whole.png"},"type":"image.stack"},"mode":"parts","params":{"scene":"spiral"},"parts":12,"type":"render.fractal"},` +
		`{"mode":"perFile","params":{"width":"800"},"type":"image.resize"},{"type":"file.hash"}]`
	if string(raw) != want {
		t.Fatalf("steps\n got %s\nwant %s", raw, want)
	}

	for _, bad := range [][]string{
		{"-combine", "archive.zip"},          // nothing to combine yet
		{"-step", "image.resize width"},      // not key=value
		{"-step", "render.fractal parts=x"},  // parts not a number
		{"-step", ""},                        // no type
		{"-step", "x", "-combine", "y once"}, // a combine has no mode
	} {
		fs := flag.NewFlagSet("workflow", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		var steps stepFlags
		fs.Var(stepFlag{&steps}, "step", "")
		fs.Var(combineFlag{&steps}, "combine", "")
		if err := fs.Parse(bad); err == nil {
			t.Errorf("%v: accepted", bad)
		}
	}
}

func TestWorkflowFileParamsBecomeStrings(t *testing.T) {
	var body map[string]any
	json.Unmarshal([]byte(`{"stages":[{"type":"image.resize","params":{"width":800,"format":"png"},"combine":{"type":"x","params":{"big":true}}}]}`), &body)
	for _, st := range body["stages"].([]any) {
		stringParams(st)
	}
	raw, _ := json.Marshal(body)
	if !strings.Contains(string(raw), `"width":"800"`) || !strings.Contains(string(raw), `"big":"true"`) || !strings.Contains(string(raw), `"format":"png"`) {
		t.Fatalf("params %s", raw)
	}
}
