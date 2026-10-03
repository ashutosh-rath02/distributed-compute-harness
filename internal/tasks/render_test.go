package tasks

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"os"
	"strings"
	"testing"
)

func renderPart(t *testing.T, part, parts int) []byte {
	t.Helper()
	env, _, err := run(t, "render.fractal", map[string]string{
		"width": "96", "height": "64", "part": fmt.Sprint(part), "parts": fmt.Sprint(parts), "iterations": "300", "scene": "seahorse",
	}, nil)
	if err != nil {
		t.Fatalf("render part %d/%d: %v", part, parts, err)
	}
	data, err := os.ReadFile(env.out(0))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func decodePNG(t *testing.T, data []byte) image.Image {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// The point of strips: rendered on different devices and stacked, they
// must make exactly the picture rendered in one piece.
func TestStackedStripsEqualTheWholeImage(t *testing.T) {
	whole := decodePNG(t, renderPart(t, 0, 1))
	if b := whole.Bounds(); b.Dx() != 96 || b.Dy() != 64 {
		t.Fatalf("whole image is %v", b)
	}
	files := map[string][]byte{}
	var names []string
	for part := 0; part < 3; part++ { // 64 rows in 3 uneven strips
		name := fmt.Sprintf("parts/%04d/fractal-%d.png", part, part)
		files[name] = renderPart(t, part, 3)
		names = append(names, name)
	}
	// Handed over in the wrong order: name order decides.
	names[0], names[2] = names[2], names[0]
	env, out, err := run(t, "image.stack", map[string]string{"name": "joined.png"}, files, names...)
	if err != nil {
		t.Fatalf("stack: %v", err)
	}
	data, _ := os.ReadFile(env.out(0))
	joined := decodePNG(t, data)
	if joined.Bounds() != whole.Bounds() || !strings.Contains(out, "stacked 3 image(s)") {
		t.Fatalf("joined %v (%q), whole %v", joined.Bounds(), out, whole.Bounds())
	}
	for y := 0; y < 64; y++ {
		for x := 0; x < 96; x++ {
			if joined.At(x, y) != whole.At(x, y) {
				t.Fatalf("pixel (%d,%d) differs: stacked strips must equal the whole image", x, y)
			}
		}
	}
}

func TestRenderRefusesImpossibleParts(t *testing.T) {
	for _, p := range []map[string]string{
		{"width": "64", "height": "64", "part": "3", "parts": "3"},        // no such part
		{"width": "64", "height": "16", "part": "0", "parts": "20"},       // more parts than rows
		{"width": "8000", "height": "8000", "part": "0", "parts": "1000"}, // over the pixel cap
	} {
		if _, _, err := run(t, "render.fractal", p, nil); err == nil {
			t.Errorf("%v: expected an error", p)
		}
	}
}
