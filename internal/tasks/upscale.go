package tasks

import (
	"context"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"os"
	"runtime"
	"sync"

	"home-harness/internal/catalog"
)

// ---- image.upscale
//
// Catmull-Rom resampling (the cubic golang.org/x/image/draw calls
// CatmullRom), written out here rather than adding that module: its
// Scale keeps a 32-byte-per-pixel buffer the size of the whole half-done
// image (640 MB for a 2x of 10 MP), while this works a band of rows at a
// time and needs little beyond the source and the result.

type imageUpscale struct{}

func (imageUpscale) Available(ctx context.Context) error { return always(ctx) }
func (imageUpscale) Run(ctx context.Context, env Env) error {
	f, err := os.Open(env.in(0))
	if err != nil {
		return err
	}
	defer f.Close()
	// Dimensions first: refuse a source or a result over the limit before
	// allocating a pixel (a few-KB file can claim enormous dimensions).
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return fmt.Errorf("not a readable image: %w", err)
	}
	if cfg.Width < 1 || cfg.Height < 1 {
		return fmt.Errorf("image is %dx%d", cfg.Width, cfg.Height)
	}
	if int64(cfg.Width)*int64(cfg.Height) > catalog.MaxImagePixels {
		return fmt.Errorf("image is %dx%d, over the %d-pixel limit", cfg.Width, cfg.Height, catalog.MaxImagePixels)
	}
	s := env.intParam("scale")
	w, h := int64(cfg.Width)*int64(s), int64(cfg.Height)*int64(s)
	if w*h > catalog.MaxImagePixels {
		return fmt.Errorf("a %dx%d result is over the %d-pixel limit", w, h, catalog.MaxImagePixels)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	src, _, err := image.Decode(f)
	if err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	dst, err := catmullRomUpscale(ctx, src, s)
	if err != nil {
		return err
	}
	o, err := os.Create(env.out(0))
	if err != nil {
		return err
	}
	switch env.param("format") {
	case "jpg":
		err = jpeg.Encode(o, dst, &jpeg.Options{Quality: env.intParam("quality")})
	default:
		err = png.Encode(o, dst)
	}
	if cerr := o.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(env.Stdout, "%s: %dx%d -> %dx%d (x%d) %s\n", env.Inputs[0], cfg.Width, cfg.Height, w, h, s, env.param("format"))
	return err
}

// catmullRom is the kernel: 1 at 0, 0 at every other whole number, with
// small negative lobes that keep edges sharp.
func catmullRom(x float64) float64 {
	x = math.Abs(x)
	switch {
	case x < 1:
		return (1.5*x-2.5)*x*x + 1
	case x < 2:
		return ((-0.5*x+2.5)*x-4)*x + 2
	}
	return 0
}

// upscaleTaps are, for each of the s output pixels one source pixel
// becomes, the offset of the first of the 4 source pixels it reads
// (relative to that source pixel) and their weights.
func upscaleTaps(s int) ([]int, [][4]float32) {
	first := make([]int, s)
	weights := make([][4]float32, s)
	for p := 0; p < s; p++ {
		// Output pixel centers sit at (p+0.5)/s - 0.5 source pixels from
		// their source pixel's center.
		at := (float64(p)+0.5)/float64(s) - 0.5
		base := int(math.Floor(at))
		frac := at - float64(base)
		first[p] = base - 1
		var sum float64
		var w [4]float64
		for i := range w {
			w[i] = catmullRom(frac - float64(i-1))
			sum += w[i]
		}
		for i := range w {
			weights[p][i] = float32(w[i] / sum)
		}
	}
	return first, weights
}

// catmullRomUpscale makes src s times wider and taller. It works in
// premultiplied alpha (a transparent pixel's color doesn't bleed into its
// neighbors), clamping what the kernel's lobes overshoot. Edges repeat
// the border pixels. Rows are done in bands, one per CPU.
func catmullRomUpscale(ctx context.Context, src image.Image, s int) (*image.RGBA, error) {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	in, ok := src.(*image.RGBA)
	if !ok || in.Rect.Min != (image.Point{}) {
		in = image.NewRGBA(image.Rect(0, 0, sw, sh))
		draw.Draw(in, in.Rect, src, b.Min, draw.Src)
	}
	dw, dh := sw*s, sh*s
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	first, weights := upscaleTaps(s)
	clampTo := func(v, n int) int { return min(max(v, 0), n-1) }

	// row returns source row y scaled horizontally (dw pixels x 4
	// channels), from a 4-row cache: the rows one output row reads are 4
	// neighbors, so they never share a slot.
	type cached struct {
		y   int
		pix []float32
	}
	newCache := func() []cached {
		c := make([]cached, 4)
		for i := range c {
			c[i] = cached{y: -1, pix: make([]float32, dw*4)}
		}
		return c
	}
	row := func(cache []cached, y int) []float32 {
		c := &cache[y%4]
		if c.y == y {
			return c.pix
		}
		c.y = y
		srow := in.Pix[y*in.Stride : y*in.Stride+sw*4]
		for x := 0; x < dw; x++ {
			q, p := x/s, x%s
			var r, g, bl, a float32
			for i, wt := range weights[p] {
				o := clampTo(q+first[p]+i, sw) * 4
				r += wt * float32(srow[o])
				g += wt * float32(srow[o+1])
				bl += wt * float32(srow[o+2])
				a += wt * float32(srow[o+3])
			}
			c.pix[x*4], c.pix[x*4+1], c.pix[x*4+2], c.pix[x*4+3] = r, g, bl, a
		}
		return c.pix
	}

	workers := max(1, min(runtime.NumCPU(), dh))
	band := (dh + workers - 1) / workers
	var wg sync.WaitGroup
	var canceled sync.Once
	var cancelErr error
	for y0 := 0; y0 < dh; y0 += band {
		y1 := min(y0+band, dh)
		wg.Add(1)
		go func() {
			defer wg.Done()
			cache := newCache()
			for y := y0; y < y1; y++ {
				if (y-y0)%64 == 0 && ctx.Err() != nil {
					canceled.Do(func() { cancelErr = ctx.Err() })
					return
				}
				q, p := y/s, y%s
				var rows [4][]float32
				for i := range rows {
					rows[i] = row(cache, clampTo(q+first[p]+i, sh))
				}
				w := weights[p]
				out := dst.Pix[y*dst.Stride : y*dst.Stride+dw*4]
				for x := 0; x < dw*4; x += 4 {
					a := w[0]*rows[0][x+3] + w[1]*rows[1][x+3] + w[2]*rows[2][x+3] + w[3]*rows[3][x+3]
					av := clamp8(a, 255)
					out[x+3] = av
					for ch := 0; ch < 3; ch++ {
						v := w[0]*rows[0][x+ch] + w[1]*rows[1][x+ch] + w[2]*rows[2][x+ch] + w[3]*rows[3][x+ch]
						out[x+ch] = clamp8(v, av) // premultiplied: never more than alpha
					}
				}
			}
		}()
	}
	wg.Wait()
	if cancelErr != nil {
		return nil, cancelErr
	}
	return dst, nil
}

// clamp8 rounds v to the nearest whole value in [0, hi].
func clamp8(v float32, hi uint8) uint8 {
	switch {
	case v <= 0:
		return 0
	case v >= float32(hi):
		return hi
	}
	return uint8(v + 0.5)
}
