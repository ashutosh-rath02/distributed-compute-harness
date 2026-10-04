package tasks

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/color"
	"image/color/palette"
	"image/gif"
	"image/jpeg"
	"os"
	"strings"
	"testing"

	"home-harness/internal/catalog"
	"home-harness/internal/domain"
)

// Types marked Deterministic in the catalog are spot-checked by running
// them again on another device and comparing output hashes: here each is
// run twice, in separate directories, and must give identical bytes —
// and the bytes this build gives are pinned (golden), so a different Go
// release or CPU architecture that encodes differently shows up here
// first rather than as honest devices flagged for a mismatch.
//
// Note what this can't show: render.fractal would pass a run-twice test
// on any one machine, yet a phone (arm64, fused multiply-adds) and a
// laptop (amd64) render different bytes. That's why it isn't marked.

type determinismCase struct {
	name   string
	typ    domain.CapabilityName
	params map[string]string
	files  map[string][]byte
	order  []string
	// golden: sha256 of each output, in output order.
	golden []string
}

func testJPEG(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(3 * x), uint8(5 * y), uint8(x ^ y), 255})
		}
	}
	var b bytes.Buffer
	jpeg.Encode(&b, img, &jpeg.Options{Quality: 90})
	return b.Bytes()
}

func testGIF(w, h int) []byte {
	img := image.NewPaletted(image.Rect(0, 0, w, h), palette.Plan9)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetColorIndex(x, y, uint8((x*7+y*3)%256))
		}
	}
	var b bytes.Buffer
	gif.Encode(&b, img, nil)
	return b.Bytes()
}

func determinismCases() []determinismCase {
	text := map[string][]byte{
		"notes.txt":    []byte("one two  three\nfour\tfive\r\nsix é ü 漢字\n\nend"),
		"more/log.txt": []byte(strings.Repeat("alpha beta\n", 500)),
	}
	png1, png2 := testPNG(64, 48), testPNG(64, 16)
	return []determinismCase{
		{"file.hash sha256", "file.hash", nil, text, []string{"notes.txt", "more/log.txt"}, []string{"0c28e0056bb36efe4a17cb86356148cf3ca5ddfb091fbff0f1ada22c4f1e53de"}},
		{"file.hash md5", "file.hash", map[string]string{"algorithm": "md5"}, text, []string{"more/log.txt", "notes.txt"}, []string{"0e5a787c50ae7cf9af87f3a4aa02a1205f7634ce71dce513959ff43cdb15adde"}},
		{"text.count", "text.count", nil, text, []string{"notes.txt", "more/log.txt"}, []string{"e9329037e285380b817ec2d35c955f148d3a26155648adbaa55417cbb781004b"}},
		{"image.resize png to jpg", "image.resize", map[string]string{"width": "40"}, map[string][]byte{"a.png": png1}, []string{"a.png"}, []string{"66d4f44781386dea5459f0d9e030dfdd3e808fdb890e9c6c001c9e2e310a547b"}},
		{"image.resize png to png", "image.resize", map[string]string{"width": "100", "format": "png"}, map[string][]byte{"a.png": png1}, []string{"a.png"}, []string{"05bdcb93a5cd118376a04eb39765bf7d3bd3fbafa276845113bfa94584cca815"}},
		{"image.resize jpeg", "image.resize", map[string]string{"width": "33", "height": "20", "quality": "70"}, map[string][]byte{"p.jpg": testJPEG(80, 60)}, []string{"p.jpg"}, []string{"16f0a1ea2c8fda8f0e4ecd4df9ef1f33408093d9b7f2fb7ca2d686a23d85aeea"}},
		{"image.resize gif", "image.resize", map[string]string{"width": "24", "format": "png"}, map[string][]byte{"g.gif": testGIF(48, 40)}, []string{"g.gif"}, []string{"f829484a130829ad8f716e873098489912d27ed9f5f7d3d88b93801dacdd2edd"}},
		{"image.stack", "image.stack", nil, map[string][]byte{"parts/0001/b.png": png2, "parts/0000/a.png": png1}, []string{"parts/0001/b.png", "parts/0000/a.png"}, []string{"0cebe7c45d2b5855ec429ada18d3050d7b7c50bb6c1b9be09cc1ad83fa25ac9b"}},
	}
}

func outputHashes(t *testing.T, env Env) []string {
	t.Helper()
	var out []string
	for i := range env.Outputs {
		data, err := os.ReadFile(env.out(i))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		out = append(out, hex.EncodeToString(sum[:]))
	}
	return out
}

func TestDeterministicTypesGiveIdenticalBytes(t *testing.T) {
	covered := map[domain.CapabilityName]bool{}
	for _, c := range determinismCases() {
		t.Run(c.name, func(t *testing.T) {
			covered[c.typ] = true
			first, _, err := run(t, c.typ, c.params, c.files, c.order...)
			if err != nil {
				t.Fatal(err)
			}
			second, _, err := run(t, c.typ, c.params, c.files, c.order...)
			if err != nil {
				t.Fatal(err)
			}
			if first.Dir == second.Dir {
				t.Fatal("the two runs must use separate directories")
			}
			a, b := outputHashes(t, first), outputHashes(t, second)
			if strings.Join(a, ",") != strings.Join(b, ",") {
				t.Fatalf("two runs differ: %v vs %v", a, b)
			}
			if strings.Join(a, ",") != strings.Join(c.golden, ",") {
				t.Errorf("output hashes %q, pinned %q: if Go's encoders or this handler changed, devices on different builds will now disagree (and spot checks between them report mismatches) until every agent runs the new build", a, c.golden)
			}
		})
	}
	for _, ty := range catalog.Types() {
		if ty.Deterministic && !covered[ty.Name] {
			t.Errorf("%s is marked deterministic but has no case here", ty.Name)
		}
	}
}
