package catalog

import (
	"errors"
	"strings"
	"testing"

	"home-harness/internal/domain"
)

// Labels are bounded in number and length, distinct, and never start with
// something an option parser or a spreadsheet would act on.
func TestLabelsAreBoundedDistinctAndCanonical(t *testing.T) {
	ty := mustType(t, "llm.classify")
	compile := func(labels string) (string, error) {
		p, _, err := ty.Compile(map[string]string{"model": "gemma3:1b", "labels": labels}, img("a.txt"))
		return p["labels"], err
	}
	if got, err := compile(" invoice , receipt,letter to the bank, Café (FR) "); err != nil || got != "invoice,receipt,letter to the bank,Café (FR)" {
		t.Fatalf("canonical labels: %q %v", got, err)
	}
	if again, err := compile("invoice,receipt,letter to the bank,Café (FR)"); err != nil || again != "invoice,receipt,letter to the bank,Café (FR)" {
		t.Fatalf("recompiling the canonical form changed it: %q %v", again, err)
	}
	var many []string
	for i := 0; i < MaxLabels; i++ {
		many = append(many, "label"+string(rune('a'+i%26))+string(rune('a'+i/26)))
	}
	if _, err := compile(strings.Join(many, ",")); err != nil {
		t.Fatalf("%d labels refused: %v", MaxLabels, err)
	}
	for name, bad := range map[string]string{
		"one label":        "spam",
		"too many":         strings.Join(many, ",") + ",extra",
		"twice":            "spam,ham,SPAM",
		"empty":            "spam,,ham",
		"too long":         "spam," + strings.Repeat("h", MaxLabelBytes+1),
		"formula":          "spam,=HYPERLINK(1)",
		"plus":             "spam,+1",
		"at":               "spam,@x",
		"leading dash":     "-spam,ham",
		"inner dash first": "spam, -ham",
		"quote":            `spam,"ham"`,
		"newline":          "spam,h\nam",
		"semicolon":        "spam,ham;eggs",
	} {
		if _, err := compile(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s (%q): got %v, want ErrInvalid", name, bad, err)
		}
	}
	if _, err := compile(strings.Repeat("a", 2049)); !errors.Is(err, ErrInvalid) {
		t.Fatal("labels over the parameter's length bound accepted")
	}
}

func TestBatchAITypes(t *testing.T) {
	classify := mustType(t, "llm.classify")
	if _, outs, err := classify.Compile(map[string]string{"model": "m", "labels": "a,b"}, img("notes.txt")); err != nil || len(outs) != 1 || outs[0] != "label.json" {
		t.Fatalf("classify: %v %v", outs, err)
	}
	if _, _, err := classify.Compile(map[string]string{"model": "m", "labels": "a,b"}, append(img("a.txt"), img("b.txt")...)); !errors.Is(err, ErrInvalid) {
		t.Fatal("classify takes exactly one file")
	}
	embed := mustType(t, "llm.embed")
	if ok, _ := embed.Offers(map[string]string{AttrModels: "embeddinggemma:latest"}, map[string]string{"model": "embeddinggemma"}); !ok {
		t.Fatal("embed: the device's embedding model")
	}
	if ok, _ := embed.Offers(map[string]string{AttrModels: "embeddinggemma:latest"}, map[string]string{"model": "gemma3:1b"}); ok {
		t.Fatal("embed placed with a model its capability doesn't list")
	}
	report := mustType(t, "report.collect")
	if !report.Reduce || !report.PartNames {
		t.Fatal("report.collect is a combine step that names its parts")
	}
	for name, want := range map[string]string{"": "report.md", "labels.csv": "labels.csv", "vectors.json": "vectors.json"} {
		params := map[string]string{}
		if name != "" {
			params["name"] = name
		}
		if _, outs, err := report.Compile(params, img("parts/0000/response.txt")); err != nil || outs[0] != want {
			t.Errorf("report name %q: %v %v", name, outs, err)
		}
	}
	for _, bad := range []string{"report.txt", "../report.md", "report", "-r.md"} {
		if _, _, err := report.Compile(map[string]string{"name": bad}, img("a.txt")); !errors.Is(err, ErrInvalid) {
			t.Errorf("report name %q accepted", bad)
		}
	}
	if _, _, err := report.Compile(nil, img("parts/0000/photo-800.jpg")); !errors.Is(err, ErrInvalid) {
		t.Fatal("report.collect over images accepted")
	}
	if err := domain.ValidArtifactName(TaskIndexName); err != nil {
		t.Fatal(err)
	}
}
