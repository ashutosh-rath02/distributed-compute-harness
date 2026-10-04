package manager

import (
	"encoding/json"
	"strings"
	"testing"

	"home-harness/internal/catalog"
	"home-harness/internal/domain"
)

func offered(t *testing.T, names ...domain.CapabilityName) []plannable {
	t.Helper()
	var out []plannable
	for _, n := range names {
		ty, ok := catalog.Lookup(n)
		if !ok {
			t.Fatalf("no catalog type %s", n)
		}
		out = append(out, plannable{Type: ty, choices: map[string][]string{"model": {"gemma3:1b"}}})
	}
	return out
}

var planFiles = []domain.ArtifactRef{
	{Name: "beach.jpg", SHA256: strings.Repeat("a", 64)},
	{Name: "city.png", SHA256: strings.Repeat("b", 64)},
}

func TestDraftPlanAcceptsTheSchemaShapeAndToleratesChattyModels(t *testing.T) {
	types := offered(t, "image.resize", "archive.zip", "render.fractal", "image.stack")
	answer := "Sure! Here is the plan:\n```json\n" + `{"summary":"Resize and zip.","task":{"type":"image.resize","params":{"width":800,"quality":90.0,"format":"png","height":null}},` +
		`"files":["beach.jpg","city.png"],"mode":"perFile","parts":0,"combine":{"type":"archive.zip","params":{"name":"small.zip"}},"possible":true,"reason":""}` + "\n```"
	d, err := draftPlan(answer, planFiles, types)
	if err != nil {
		t.Fatal(err)
	}
	if d.task.Type != "image.resize" || d.task.Params["width"] != "800" || d.task.Params["quality"] != "90" || d.task.Params["format"] != "png" {
		t.Fatalf("task %+v", d.task)
	}
	if _, set := d.task.Params["height"]; set {
		t.Fatal("a null parameter is left to its default")
	}
	if d.mode != planPerFile || len(d.files) != 2 || d.combine == nil || d.combine.Params["name"] != "small.zip" {
		t.Fatalf("draft %+v", d)
	}
	spec := d.jobSpec(planFiles)
	if len(spec.Tasks) != 2 || spec.Tasks[0].Inputs[0].Name != "beach.jpg" || spec.Tasks[1].Inputs[0].Name != "city.png" || spec.Reduce == nil || spec.Reduce.Capability != "archive.zip" {
		t.Fatalf("spec %+v", spec)
	}
	// Each task gets its own copy of the parameters.
	spec.Tasks[0].Params["width"] = "1"
	if spec.Tasks[1].Params["width"] != "800" || d.task.Params["width"] != "800" {
		t.Fatal("tasks share a parameter map")
	}

	// No files named with files given: a type that takes files gets all.
	d, err = draftPlan(`{"task":{"type":"image.resize","params":{"width":"100"}},"files":[],"mode":"perFile","combine":null}`, planFiles, types)
	if err != nil || len(d.files) != 2 {
		t.Fatalf("default files: %v %+v", err, d)
	}

	// Only one reading: a type taking one file per run, given several,
	// runs once per file; once per file for a type that takes no files
	// runs once.
	d, err = draftPlan(`{"task":{"type":"image.resize","params":{"width":"100"}},"files":["beach.jpg","city.png"],"mode":"once"}`, planFiles, types)
	if err != nil || d.mode != planPerFile {
		t.Fatalf("one-file type, several files: %v %+v", err, d)
	}
	d, err = draftPlan(`{"task":{"type":"image.resize","params":{"width":"100"}},"files":["beach.jpg"],"mode":"once"}`, planFiles, types)
	if err != nil || d.mode != planOnce {
		t.Fatalf("one-file type, one file: %v %+v", err, d)
	}
	d, err = draftPlan(`{"task":{"type":"render.fractal","params":{}},"files":[],"mode":"perFile","parts":1}`, nil, types)
	if err != nil || d.mode != planOnce {
		t.Fatalf("perFile without files: %v %+v", err, d)
	}
	d, err = draftPlan(`{"task":{"type":"render.fractal","params":{}},"files":[],"mode":"once","parts":6}`, nil, types)
	if err != nil || d.mode != planParts || d.parts != 6 {
		t.Fatalf("parts given, mode not: %v %+v", err, d)
	}

	// A strip renderer asked for in N parts runs N times, whatever mode
	// the model chose; part/parts are the plan's to set.
	d, err = draftPlan(`{"task":{"type":"render.fractal","params":{"parts":8,"part":3,"scene":"spiral"}},"files":[],"mode":"once","parts":0,"combine":{"type":"image.stack","params":{}}}`, nil, types)
	if err != nil || d.mode != planParts || d.parts != 8 {
		t.Fatalf("parts: %v %+v", err, d)
	}
	spec = d.jobSpec(nil)
	if len(spec.Tasks) != 8 || spec.Tasks[7].Params["part"] != "7" || spec.Tasks[7].Params["parts"] != "8" || spec.Tasks[0].Params["scene"] != "spiral" {
		t.Fatalf("parts spec %+v", spec.Tasks)
	}
}

func TestDraftPlanRefusesWhatWasntOffered(t *testing.T) {
	types := offered(t, "image.resize", "archive.zip", "file.hash")
	for name, tc := range map[string]struct{ answer, want string }{
		"not json":             {"I would resize the photos.", "isn't JSON"},
		"wrong shape":          {`{"task":"image.resize"}`, "isn't the JSON asked for"},
		"no task":              {`{"summary":"x","task":null}`, "names no task"},
		"raw command":          {`{"task":{"type":"system.execute","params":{"command":"rm"}}}`, `"system.execute" isn't a task type`},
		"internal type":        {`{"task":{"type":"llm.split-main","params":{}}}`, "isn't a task type"},
		"not offered":          {`{"task":{"type":"render.fractal","params":{}}}`, "isn't a task type"},
		"nested param":         {`{"task":{"type":"image.resize","params":{"width":{"px":5}}}}`, "single value"},
		"file not given":       {`{"task":{"type":"image.resize","params":{}},"files":["../secret.jpg"],"mode":"perFile"}`, "isn't one of the files given"},
		"file twice":           {`{"task":{"type":"image.resize","params":{}},"files":["beach.jpg","beach.jpg"],"mode":"perFile"}`, "twice"},
		"unknown mode":         {`{"task":{"type":"file.hash","params":{}},"mode":"everywhere"}`, "unknown mode"},
		"parts on a non-parts": {`{"task":{"type":"file.hash","params":{}},"mode":"parts","parts":3}`, "can't be split"},
		"combine not a reduce": {`{"task":{"type":"file.hash","params":{}},"mode":"once","combine":{"type":"image.resize","params":{}}}`, "combine results"},
		"bad parts":            {`{"task":{"type":"file.hash","params":{}},"mode":"once","parts":"many"}`, "whole number"},
	} {
		if _, err := draftPlan(tc.answer, planFiles, types); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
	// The model declining is not an error: the plan shows its reason.
	d, err := draftPlan(`{"summary":"","task":{"type":"file.hash","params":{}},"files":[],"mode":"once","parts":0,"combine":null,"possible":false,"reason":"No task type sends email."}`, planFiles, types)
	if err != nil || d.possible || d.reason != "No task type sends email." {
		t.Fatalf("declined: %v %+v", err, d)
	}
	// perFile needs files, and a type that takes them.
	if _, err := draftPlan(`{"task":{"type":"image.resize","params":{}},"mode":"perFile"}`, nil, types); err == nil || !strings.Contains(err.Error(), "no files") {
		t.Fatalf("perFile without files: %v", err)
	}
	types = offered(t, "cpu.burn")
	if d, err := draftPlan(`{"task":{"type":"cpu.burn","params":{}},"mode":"perFile"}`, planFiles, types); err != nil || d.mode != planOnce || len(d.files) != 0 {
		t.Fatalf("perFile on a type without files runs once, with none: %v %+v", err, d)
	}
}

func TestPlanSchemaKeepsItsOrderAndOffersOnlyTheGivenChoices(t *testing.T) {
	types := offered(t, "image.resize", "archive.zip", "llm.generate")
	schema := planSchema(types, []string{"a.jpg", `we"ird.png`})
	if !json.Valid([]byte(schema)) {
		t.Fatalf("schema isn't JSON: %s", schema)
	}
	// The model writes keys in the schema's order: the plan's substance
	// before the yes/no.
	last := -1
	for _, key := range []string{`"summary"`, `"task"`, `"files"`, `"mode"`, `"parts"`, `"combine"`, `"possible"`, `"reason"`} {
		i := strings.Index(schema, key)
		if i < last {
			t.Fatalf("%s out of order in %s", key, schema)
		}
		last = i
	}
	// The canonical form an agent receives keeps that order too.
	ty, _ := catalog.Lookup("llm.chat")
	params, _, err := ty.Compile(map[string]string{"model": "m", "messages": `[{"role":"user","content":"x"}]`, "format": schema}, nil)
	if err != nil || strings.Index(params["format"], `"summary"`) > strings.Index(params["format"], `"possible"`) {
		t.Fatalf("compiled format: %v %s", err, params["format"])
	}
	type stepJSON struct {
		Properties struct {
			Type   struct{ Enum []string }
			Params struct {
				Properties           map[string]json.RawMessage
				AdditionalProperties *bool
			}
		}
	}
	var s struct {
		Properties struct {
			Task  stepJSON
			Files struct {
				Items    struct{ Enum []string }
				MaxItems *int
			}
			Combine struct{ AnyOf []stepJSON }
		}
	}
	decode := func(schema string) {
		s.Properties.Files.MaxItems = nil
		if err := json.Unmarshal([]byte(schema), &s); err != nil {
			t.Fatal(err)
		}
	}
	decode(schema)
	task := s.Properties.Task.Properties
	if got := strings.Join(task.Type.Enum, ","); got != "image.resize,archive.zip,llm.generate" {
		t.Fatalf("task types %s", got)
	}
	// Params: exactly the offered types' own, typed, nothing else (a
	// model once put "files" there).
	if a := task.Params.AdditionalProperties; a == nil || *a {
		t.Fatal("params must allow nothing beyond the types' own")
	}
	props := task.Params.Properties
	for name, want := range map[string]string{
		"width":   `{"type":"integer","minimum":1,"maximum":10000}`,
		"format":  `{"type":"string","enum":["jpg","png"]}`,
		"model":   `{"type":"string","enum":["gemma3:1b"]}`,
		"name":    `{"type":"string"}`,
		"prompt":  `{"type":"string"}`,
		"seed":    `{"type":"integer","minimum":0,"maximum":2147483647}`,
		"quality": `{"type":"integer","minimum":1,"maximum":100}`,
	} {
		if string(props[name]) != want {
			t.Errorf("param %s: %s, want %s", name, props[name], want)
		}
	}
	if _, ok := props["files"]; ok || len(props) != 4+1+6 {
		t.Errorf("params %v", props)
	}
	if got := strings.Join(s.Properties.Files.Items.Enum, ","); got != `a.jpg,we"ird.png` {
		t.Fatalf("files %s", got)
	}
	if len(s.Properties.Combine.AnyOf) != 2 || strings.Join(s.Properties.Combine.AnyOf[1].Properties.Type.Enum, ",") != "archive.zip" {
		t.Fatalf("combine %+v", s.Properties.Combine)
	}
	if cp := s.Properties.Combine.AnyOf[1].Properties.Params.Properties; len(cp) != 1 || cp["name"] == nil {
		t.Fatalf("combine params %v", cp)
	}

	// The same parameter name with another range in another type: any
	// basic value (the catalog checks it); a Parts type's counters are
	// the plan's, not the model's; no files given, none can be named;
	// nothing to combine with, combine can only be null.
	decode(planSchema(offered(t, "image.resize", "render.fractal"), nil))
	props = s.Properties.Task.Properties.Params.Properties
	if string(props["width"]) != `{"type":["string","number","boolean"]}` || props["part"] != nil || props["parts"] != nil || props["scene"] == nil {
		t.Fatalf("merged params %v", props)
	}
	if m := s.Properties.Files.MaxItems; m == nil || *m != 0 {
		t.Fatal("with no files given, files must be empty")
	}
	if !strings.Contains(planSchema(offered(t, "image.resize"), nil), `"combine":{"type":"null"}`) {
		t.Fatal("combine without combiners")
	}

	prompt := planPrompt(types, []string{"a.jpg"})
	for _, want := range []string{"image.resize: Resize an image", "archive.zip [can combine]", "width (Width (pixels)): whole number 1-10000, default 1024", "model (Model): one of gemma3:1b, required", "The user's files: a.jpg"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "system.execute") || strings.Contains(prompt, "Example") {
		t.Errorf("prompt offers more than it was given:\n%s", prompt)
	}
}
