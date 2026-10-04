package tasks

import (
	"context"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"math"
	"os"
	"strings"
	"testing"
	"time"
)

func TestUpscaleTapWeightsSumToOne(t *testing.T) {
	for s := 2; s <= 4; s++ {
		_, weights := upscaleTaps(s)
		for p, w := range weights {
			if sum := w[0] + w[1] + w[2] + w[3]; math.Abs(float64(sum)-1) > 1e-6 {
				t.Fatalf("scale %d phase %d: weights %v sum to %v", s, p, w, sum)
			}
		}
	}
	// Catmull-Rom passes through the samples and is zero at the others.
	if catmullRom(0) != 1 || catmullRom(1) != 0 || catmullRom(-2) != 0 || catmullRom(2.5) != 0 {
		t.Fatal("kernel")
	}
}

// A flat image stays flat; a linear ramp stays a ramp (Catmull-Rom is
// exact for straight lines) away from the edges, which repeat the border.
func TestUpscaleKeepsFlatAndLinearImages(t *testing.T) {
	flat := image.NewRGBA(image.Rect(0, 0, 7, 5))
	for i := 0; i < len(flat.Pix); i += 4 {
		copy(flat.Pix[i:], []uint8{200, 100, 50, 255})
	}
	dst, err := catmullRomUpscale(context.Background(), flat, 3)
	if err != nil {
		t.Fatal(err)
	}
	if dst.Rect.Dx() != 21 || dst.Rect.Dy() != 15 {
		t.Fatalf("size %v", dst.Rect)
	}
	for i := 0; i < len(dst.Pix); i += 4 {
		if dst.Pix[i] != 200 || dst.Pix[i+1] != 100 || dst.Pix[i+2] != 50 || dst.Pix[i+3] != 255 {
			t.Fatalf("flat image changed at %d: %v", i/4, dst.Pix[i:i+4])
		}
	}

	// Red = 10 * x, every row the same.
	ramp := image.NewRGBA(image.Rect(0, 0, 16, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 16; x++ {
			ramp.SetRGBA(x, y, color.RGBA{uint8(10 * x), 0, 0, 255})
		}
	}
	for s := 2; s <= 4; s++ {
		dst, err := catmullRomUpscale(context.Background(), ramp, s)
		if err != nil {
			t.Fatal(err)
		}
		for x := 0; x < 16*s; x++ {
			at := (float64(x)+0.5)/float64(s) - 0.5 // in source pixels
			got := float64(dst.RGBAAt(x, 2*s).R)
			switch {
			case at >= 1 && at <= 14: // every tap inside the image
				if math.Abs(got-10*at) > 0.51 {
					t.Fatalf("scale %d: x=%d (source %.3f) is %v, want %.2f", s, x, at, got, 10*at)
				}
			case at < 0:
				if got != 0 {
					t.Fatalf("scale %d: left edge x=%d is %v, want the border's 0", s, x, got)
				}
			}
		}
	}
}

// The kernel's lobes overshoot at sharp edges: the result must still be
// valid premultiplied color (no channel above alpha).
func TestUpscaleClampsOvershootToValidColor(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 4; x < 8; x++ {
			src.SetRGBA(x, y, color.RGBA{255, 255, 255, 255}) // opaque white next to transparent
		}
	}
	dst, err := catmullRomUpscale(context.Background(), src, 4)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < len(dst.Pix); i += 4 {
		if a := dst.Pix[i+3]; dst.Pix[i] > a || dst.Pix[i+1] > a || dst.Pix[i+2] > a {
			t.Fatalf("pixel %d is %v: color above alpha", i/4, dst.Pix[i:i+4])
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := catmullRomUpscale(ctx, src, 2); err == nil {
		t.Fatal("ignored cancellation")
	}
}

func TestImageUpscaleWritesTheEnlargedImage(t *testing.T) {
	env, out, err := run(t, "image.upscale", map[string]string{"scale": "3"}, map[string][]byte{"photos/cat.png": testPNG(40, 30)}, "photos/cat.png")
	if err != nil {
		t.Fatal(err)
	}
	if env.Outputs[0] != "cat-x3.png" || !strings.Contains(out, "40x30 -> 120x90") {
		t.Fatalf("output %q, stdout %q", env.Outputs[0], out)
	}
	f, _ := os.Open(env.out(0))
	cfg, format, err := image.DecodeConfig(f)
	f.Close()
	if err != nil || format != "png" || cfg.Width != 120 || cfg.Height != 90 {
		t.Fatalf("upscaled: %v %s %dx%d", err, format, cfg.Width, cfg.Height)
	}
	env, _, err = run(t, "image.upscale", map[string]string{"scale": "2", "format": "jpg"}, map[string][]byte{"a.png": testPNG(10, 10)}, "a.png")
	if err != nil {
		t.Fatal(err)
	}
	f, _ = os.Open(env.out(0))
	cfg, format, err = image.DecodeConfig(f)
	f.Close()
	if err != nil || format != "jpeg" || cfg.Width != 20 || env.Outputs[0] != "a-x2.jpg" {
		t.Fatalf("jpg: %v %s %dx%d %s", err, format, cfg.Width, cfg.Height, env.Outputs[0])
	}
}

// Both the source and the result are capped from the header alone,
// before any pixel is allocated.
func TestImageUpscaleRefusesOversizedImages(t *testing.T) {
	claim := func(w, h uint32) []byte {
		data := testPNG(1, 1)
		binary.BigEndian.PutUint32(data[16:], w)
		binary.BigEndian.PutUint32(data[20:], h)
		binary.BigEndian.PutUint32(data[29:], crc32.ChecksumIEEE(data[12:29]))
		return data
	}
	start := time.Now()
	// 50000x50000: the source itself is a bomb.
	if _, _, err := run(t, "image.upscale", nil, map[string][]byte{"bomb.png": claim(50000, 50000)}, "bomb.png"); err == nil || !strings.Contains(err.Error(), "image is 50000x50000, over") {
		t.Fatalf("bomb: %v", err)
	}
	// 5000x3000 is fine to read, but twice that is 60 MP.
	if _, _, err := run(t, "image.upscale", nil, map[string][]byte{"big.png": claim(5000, 3000)}, "big.png"); err == nil || !strings.Contains(err.Error(), "10000x6000 result is over") {
		t.Fatalf("oversized result: %v", err)
	}
	// 3000x3000 x2 = 36 MP is allowed through the checks (and then fails
	// to decode: there are no pixels).
	if _, _, err := run(t, "image.upscale", nil, map[string][]byte{"ok.png": claim(3000, 3000)}, "ok.png"); err == nil || strings.Contains(err.Error(), "pixel limit") {
		t.Fatalf("36 MP result: %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("an oversized image was decoded before being refused")
	}
}
