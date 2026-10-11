package catalog

import (
	"strings"
	"testing"

	"home-harness/internal/domain"
)

// FuzzCompile feeds every catalog type parameters and input names built
// from the fuzz data. Compile must not panic, and whatever it accepts must
// yield only valid output file names (they become paths on an agent).
func FuzzCompile(f *testing.F) {
	f.Add(uint8(0), "seconds=2\nmodel=gemma3:1b", "a.png,b.png")
	f.Add(uint8(3), "width=100\nheight=-1", "photo.jpg")
	f.Add(uint8(5), "names=a.txt,../b", "")
	f.Add(uint8(9), "\n\n=\n==\nx", "x/y/z/w/v.txt")
	f.Fuzz(func(t *testing.T, which uint8, params, inputs string) {
		types := Types()
		ty := types[int(which)%len(types)]
		p := map[string]string{}
		for _, line := range strings.Split(params, "\n") {
			if k, v, ok := strings.Cut(line, "="); ok {
				p[k] = v
			}
		}
		var in []domain.ArtifactRef
		if inputs != "" {
			for i, name := range strings.Split(inputs, ",") {
				in = append(in, domain.ArtifactRef{Name: name, SHA256: strings.Repeat("0", 63) + string(rune('0'+i%10))})
			}
		}
		_, outputs, err := ty.Compile(p, in)
		if err != nil {
			return
		}
		for _, o := range outputs {
			if verr := domain.ValidArtifactName(o); verr != nil {
				t.Fatalf("%s compiled to an invalid output name %q: %v (params %q inputs %q)", ty.Name, o, verr, params, inputs)
			}
		}
	})
}
