package catalog

import (
	"errors"
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
		params, outputs, err := ty.Compile(nil, inputs)
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
