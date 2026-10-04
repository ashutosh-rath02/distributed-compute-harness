package manager

import (
	"encoding/json"
	"strings"
	"testing"

	"home-harness/internal/domain"
)

func TestDraftPlanReadsFollowUpSteps(t *testing.T) {
	types := offered(t, "image.resize", "archive.zip", "file.hash", "render.fractal", "image.stack")
	d, err := draftPlan(`{"summary":"Resize, zip, checksum.","task":{"type":"image.resize","params":{"width":800}},"files":["beach.jpg","city.png"],"mode":"perFile","parts":0,"combine":null,`+
		`"then":[{"type":"archive.zip","params":{"name":"small.zip"},"mode":"once"},{"type":"file.hash","params":{},"mode":""}],"possible":true,"reason":""}`, planFiles, types)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.then) != 2 || d.then[0].Type != "archive.zip" || d.then[0].Params["name"] != "small.zip" || d.then[0].Mode != planOnce || d.then[1].Mode != planOnce || d.then[1].Title != "Checksum files" {
		t.Fatalf("then %+v", d.then)
	}
	spec := d.workflowSpec(planFiles)
	if len(spec.Stages) != 3 || len(spec.Inputs) != 2 || spec.Stages[0].Mode != domain.StagePerFile || spec.Stages[0].Combine != nil ||
		spec.Stages[1].Type != "archive.zip" || spec.Name != "Planned: Resize an image, then zip files together, then checksum files" {
		t.Fatalf("workflow %+v", spec)
	}
	// The plan's own copy of the parameters is never the workflow's.
	spec.Stages[1].Params["name"] = "x.zip"
	if d.then[0].Params["name"] != "small.zip" {
		t.Fatal("the workflow shares the plan's parameters")
	}

	// A follow-up that takes one file per run can only run once per file;
	// strips in parts, stacked, then resized.
	d, err = draftPlan(`{"task":{"type":"render.fractal","params":{"scene":"spiral"}},"files":[],"mode":"parts","parts":8,"combine":{"type":"image.stack","params":{}},`+
		`"then":[{"type":"image.resize","params":{"width":400},"mode":"once"}],"possible":true}`, nil, types)
	if err != nil || len(d.then) != 1 || d.then[0].Mode != planPerFile || d.mode != planParts || d.parts != 8 {
		t.Fatalf("strips: %v %+v", err, d)
	}
	if spec := d.workflowSpec(nil); spec.Stages[0].Parts != 8 || spec.Stages[0].Combine == nil || spec.Stages[0].Combine.Type != "image.stack" {
		t.Fatalf("strips workflow %+v", spec.Stages[0])
	}

	// No follow-ups: the one-step plan it always was.
	d, err = draftPlan(`{"task":{"type":"file.hash","params":{}},"files":[],"mode":"once","combine":null,"then":[],"possible":true}`, planFiles, types)
	if err != nil || len(d.then) != 0 {
		t.Fatalf("no then: %v %+v", err, d)
	}
	d, err = draftPlan(`{"task":{"type":"file.hash","params":{}},"files":[],"mode":"once","combine":null,"then":null,"possible":true}`, planFiles, types)
	if err != nil || len(d.then) != 0 {
		t.Fatalf("then null: %v %+v", err, d)
	}
}

func TestDraftPlanRefusesFollowUpsBeyondWhatWasOffered(t *testing.T) {
	types := offered(t, "image.resize", "archive.zip", "file.hash", "render.fractal")
	step := `{"type":"file.hash","params":{},"mode":"once"}`
	for name, tc := range map[string]struct{ then, want string }{
		"too many":        {"[" + strings.Repeat(step+",", maxPlanSteps-1) + step + "]", "the plan has 5 steps; at most 4"},
		"raw command":     {`[{"type":"system.execute","params":{},"mode":"once"}]`, `step 2: "system.execute" isn't a task type the planner may use`},
		"not offered":     {`[{"type":"text.count","params":{},"mode":"once"}]`, "isn't a task type the planner may use"},
		"takes no files":  {`[{"type":"render.fractal","params":{},"mode":"once"}]`, "step 2: render.fractal takes no files"},
		"no type":         {`[{"type":"","params":{},"mode":"once"}]`, "step 2 names no task type"},
		"unknown mode":    {`[{"type":"file.hash","params":{},"mode":"parts"}]`, `step 2: unknown mode "parts"`},
		"nested param":    {`[` + step + `,{"type":"archive.zip","params":{"name":["a"]},"mode":"once"}]`, "step 3: archive.zip parameter \"name\" must be a single value"},
		"type not a step": {`[{"type":{"x":1}}]`, "isn't the JSON asked for"},
	} {
		answer := `{"task":{"type":"image.resize","params":{"width":100}},"files":["beach.jpg"],"mode":"perFile","combine":null,"then":` + tc.then + `,"possible":true}`
		if _, err := draftPlan(answer, planFiles, types); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
}

func TestPlanSchemaOffersFollowUpsThatTakeFiles(t *testing.T) {
	schema := planSchema(offered(t, "image.resize", "archive.zip", "render.fractal"), []string{"a.jpg"})
	if !json.Valid([]byte(schema)) {
		t.Fatalf("schema isn't JSON: %s", schema)
	}
	if c, th, p := strings.Index(schema, `"combine":`), strings.Index(schema, `"then":`), strings.Index(schema, `"possible":`); !(c < th && th < p) {
		t.Fatalf("then is not between combine and possible: %s", schema)
	}
	var s struct {
		Properties struct {
			Then struct {
				MaxItems *int
				Items    struct {
					Properties struct {
						Type struct{ Enum []string }
						Mode struct{ Enum []string }
					}
					Required []string
				}
			}
		}
		Required []string
	}
	if err := json.Unmarshal([]byte(schema), &s); err != nil {
		t.Fatal(err)
	}
	then := s.Properties.Then
	if then.MaxItems == nil || *then.MaxItems != maxPlanSteps-1 {
		t.Fatalf("then maxItems %v", then.MaxItems)
	}
	if got := strings.Join(then.Items.Properties.Type.Enum, ","); got != "image.resize,archive.zip" {
		t.Fatalf("follow-up types %s (render.fractal takes no files)", got)
	}
	if strings.Join(then.Items.Properties.Mode.Enum, ",") != "once,perFile" || strings.Join(then.Items.Required, ",") != "type,params,mode" {
		t.Fatalf("follow-up step %+v", then.Items)
	}
	if !strings.Contains(strings.Join(s.Required, ","), "combine,then,possible") {
		t.Fatalf("required %v", s.Required)
	}
	// The task's own step is unchanged: no mode inside it.
	if strings.Contains(stepSchema(offered(t, "image.resize"), ""), `"mode"`) {
		t.Fatal("the task step gained a mode")
	}
	if !strings.Contains(planSchema(offered(t, "render.fractal"), nil), `"then":{"type":"array","maxItems":0}`) {
		t.Fatal("follow-ups offered with no type that takes files")
	}
	if prompt := planPrompt(offered(t, "image.resize"), nil); !strings.Contains(prompt, "- then: [] unless the request asks for more steps") || !strings.Contains(prompt, "up to 3 steps") {
		t.Fatalf("prompt:\n%s", prompt)
	}
}
