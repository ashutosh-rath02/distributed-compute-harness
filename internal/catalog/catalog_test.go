package catalog

import (
	"errors"
	"regexp"
	"strings"
	"testing"

	"home-harness/internal/domain"
)

func mustType(t *testing.T, name domain.CapabilityName) Type {
	t.Helper()
	ty, ok := Lookup(name)
	if !ok {
		t.Fatalf("no catalog type %s", name)
	}
	return ty
}

func img(name string) []domain.ArtifactRef {
	return []domain.ArtifactRef{{Name: name, SHA256: strings.Repeat("0", 64)}}
}

// Every pattern compiles: a bad one would otherwise only show up as a
// panic on the first request that uses it (Go caps repeat counts at 1000).
func TestEveryParamPatternCompiles(t *testing.T) {
	for _, ty := range Types() {
		for _, p := range ty.Params {
			if p.Pattern == "" {
				continue
			}
			if _, err := regexp.Compile(`^(?:` + p.Pattern + `)$`); err != nil {
				t.Errorf("%s.%s: %v", ty.Name, p.Name, err)
			}
		}
	}
}

func TestCompileFillsDefaultsCanonicalizesAndRendersOutputs(t *testing.T) {
	ty := mustType(t, "image.resize")
	params, outputs, err := ty.Compile(map[string]string{"width": " 0800 "}, img("photos/IMG_1.JPG"))
	if err != nil {
		t.Fatal(err)
	}
	if params["width"] != "800" || params["format"] != "jpg" || params["quality"] != "85" || params["height"] != "0" {
		t.Fatalf("params = %v", params)
	}
	if len(outputs) != 1 || outputs[0] != "IMG_1-800.jpg" {
		t.Fatalf("outputs = %v", outputs)
	}
	if _, outputs, _ := ty.Compile(map[string]string{"width": "64", "format": "png"}, img("a.png")); outputs[0] != "a-64.png" {
		t.Fatalf("png output = %v", outputs)
	}
}

func TestCompileRejectsWhatTheSchemaForbids(t *testing.T) {
	resize := mustType(t, "image.resize")
	zip := mustType(t, "archive.zip")
	burn := mustType(t, "cpu.burn")
	for _, c := range []struct {
		name   string
		ty     Type
		params map[string]string
		inputs []domain.ArtifactRef
	}{
		{"unknown param", resize, map[string]string{"width": "10", "colour": "red"}, img("a.jpg")},
		{"not a number", resize, map[string]string{"width": "wide"}, img("a.jpg")},
		{"below min", resize, map[string]string{"width": "0"}, img("a.jpg")},
		{"above max", resize, map[string]string{"width": "99999"}, img("a.jpg")},
		{"not in enum", resize, map[string]string{"width": "10", "format": "bmp"}, img("a.jpg")},
		{"wrong extension", resize, map[string]string{"width": "10"}, img("a.txt")},
		{"no input", resize, map[string]string{"width": "10"}, nil},
		{"two inputs", resize, map[string]string{"width": "10"}, append(img("a.jpg"), img("b.jpg")...)},
		{"input on a no-input type", burn, nil, img("a.jpg")},
		{"pattern mismatch", zip, map[string]string{"name": "../x.zip"}, img("a")},
		{"pattern mismatch 2", zip, map[string]string{"name": "photos.tar"}, img("a")},
		{"leading dash", zip, map[string]string{"name": "-rf.zip"}, img("a")},
		{"control char", zip, map[string]string{"name": "a\n.zip"}, img("a")},
	} {
		if _, _, err := c.ty.Compile(c.params, c.inputs); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: got %v, want ErrInvalid", c.name, err)
		}
	}
}

func TestEveryBuiltinCompilesWithDefaults(t *testing.T) {
	for _, ty := range Types() {
		var inputs []domain.ArtifactRef
		for i := 0; i < ty.Inputs.Min; i++ {
			ext := "txt"
			if len(ty.Inputs.Extensions) > 0 {
				ext = ty.Inputs.Extensions[0]
			}
			inputs = append(inputs, domain.ArtifactRef{Name: "in" + string(rune('a'+i)) + "." + ext, SHA256: strings.Repeat("0", 64)})
		}
		given := map[string]string{}
		for _, p := range ty.Params {
			if p.Required && p.Default == "" {
				switch {
				case p.Type == Int || p.Type == Number:
					given[p.Name] = "1"
				case p.Pattern == SessionPattern:
					given[p.Name] = strings.Repeat("a", 16)
				case strings.Contains(p.Pattern, "gguf"):
					given[p.Name] = "model.gguf"
				default:
					given[p.Name] = "x"
				}
			}
		}
		params, outputs, err := ty.Compile(given, inputs)
		if err != nil {
			t.Errorf("%s: %v", ty.Name, err)
			continue
		}
		if err := domain.ValidateWorkloadFiles(inputs, outputs); err != nil {
			t.Errorf("%s: compiled files invalid: %v", ty.Name, err)
		}
		if ty.Version == "" || ty.Title == "" {
			t.Errorf("%s: missing version or title", ty.Name)
		}
		// Compiling the compiled params again changes nothing (the agent
		// re-validates exactly what the manager produced).
		again, outs2, err := ty.Compile(params, inputs)
		if err != nil || len(again) != len(params) || strings.Join(outs2, ",") != strings.Join(outputs, ",") {
			t.Errorf("%s: recompile differs: %v %v %v", ty.Name, again, outs2, err)
		}
	}
	if IsRaw("image.resize") || !IsRaw(domain.CapabilitySystemExecute) || !IsRaw(domain.CapabilityFilesystemRead) {
		t.Fatal("IsRaw classification")
	}
}

func TestPromptsMayBeMultilineAndStartWithADash(t *testing.T) {
	ty := mustType(t, "llm.generate")
	prompt := "- list the steps\n\t1. first\r\n2. second"
	params, outputs, err := ty.Compile(map[string]string{"model": "llama3.2", "prompt": prompt}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if params["prompt"] != prompt || params["max_tokens"] != "512" || len(outputs) != 1 || outputs[0] != "response.txt" {
		t.Fatalf("params %v outputs %v", params, outputs)
	}
	for _, bad := range []map[string]string{
		{"model": "-rf", "prompt": "x"},                              // a model is still never dash-led
		{"model": "llama3.2", "prompt": "a" + string(rune(0)) + "b"}, // NUL is still refused
		{"model": "llama 3", "prompt": "x"},                          // not a model reference
		{"model": "llama3.2", "prompt": strings.Repeat("p", 16<<10+1)},
	} {
		if _, _, err := ty.Compile(bad, nil); !errors.Is(err, ErrInvalid) {
			t.Errorf("%q: accepted", bad)
		}
	}
}

func TestModelNormalizationAndMatching(t *testing.T) {
	for in, want := range map[string]string{
		"llama3.2": "llama3.2:latest", "Llama3.2:3B": "llama3.2:3b", "hf.co/User/Repo": "hf.co/user/repo:latest",
		"hf.co/u/r:q4": "hf.co/u/r:q4", "registry:5000/m": "registry:5000/m:latest",
	} {
		if got := NormalizeModel(in); got != want {
			t.Errorf("NormalizeModel(%q) = %q, want %q", in, got, want)
		}
	}
	ty := mustType(t, "llm.generate")
	attrs := map[string]string{AttrModels: "llama3.2:latest,qwen2.5:7b"}
	if ok, _ := ty.Offers(attrs, map[string]string{"model": "llama3.2"}); !ok {
		t.Fatal("llama3.2 should match llama3.2:latest")
	}
	if ok, why := ty.Offers(attrs, map[string]string{"model": "qwen2.5"}); ok || !strings.Contains(why, "qwen2.5") {
		t.Fatalf("qwen2.5 (:latest) must not match qwen2.5:7b: %v %s", ok, why)
	}
	if ok, _ := mustType(t, "image.resize").Offers(nil, map[string]string{"width": "1"}); !ok || mustType(t, "image.resize").HasChoices() {
		t.Fatal("a type without choice params is offered by any node")
	}
}

// A parameter newer than its type goes only to agents that advertise it:
// an older agent would refuse the unknown parameter.
func TestNeedsKeepsANewParameterFromOlderAgents(t *testing.T) {
	ty := mustType(t, "llm.chat")
	if !ty.HasChoices() {
		t.Fatal("llm.chat must be matched against node attributes")
	}
	old := map[string]string{AttrModels: "gemma3:1b"}
	updated := map[string]string{AttrModels: "gemma3:1b", AttrChatFormat: "1"}
	plain := map[string]string{"model": "gemma3:1b", "messages": "[]"}
	structured := map[string]string{"model": "gemma3:1b", "messages": "[]", "format": "json"}
	if ok, _ := ty.Offers(old, plain); !ok {
		t.Fatal("an older agent still takes plain chats")
	}
	if ok, why := ty.Offers(old, structured); ok || !strings.Contains(why, "format") {
		t.Fatalf("an older agent offered a format: %v %q", ok, why)
	}
	if ok, _ := ty.Offers(updated, structured); !ok {
		t.Fatal("an updated agent takes a format")
	}
}

func TestAnswerFormatIsJSONOrASchema(t *testing.T) {
	ty := mustType(t, "llm.chat")
	base := func(format string) map[string]string {
		return map[string]string{"model": "m", "messages": `[{"role":"user","content":"x"}]`, "format": format}
	}
	for format, want := range map[string]string{
		"json":                            "json",
		" { \"b\": 1,\n \"a\": [1, 2] } ": `{"b":1,"a":[1,2]}`, // compacted, order kept
	} {
		p, _, err := ty.Compile(base(format), nil)
		if err != nil || p["format"] != want {
			t.Errorf("%q: %v %q, want %q", format, err, p["format"], want)
		}
	}
	for _, bad := range []string{"yaml", "[1,2]", `{"a":`, `"json"`} {
		if _, _, err := ty.Compile(base(bad), nil); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if p, _, err := ty.Compile(base(""), nil); err != nil || p["format"] != "" {
		t.Fatalf("no format: %v %v", err, p)
	}
	if !mustType(t, "render.fractal").Parts {
		t.Fatal("render.fractal renders one part of N")
	}
	for _, typ := range Types() {
		if !typ.Parts {
			continue
		}
		have := map[string]ParamType{}
		for _, p := range typ.Params {
			have[p.Name] = p.Type
		}
		if have["part"] != Int || have["parts"] != Int {
			t.Errorf("%s is Parts without int part/parts params", typ.Name)
		}
	}
}
