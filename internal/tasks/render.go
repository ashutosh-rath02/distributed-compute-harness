package tasks

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"os"
	"runtime"
	"sort"
	"sync"

	"home-harness/internal/catalog"
)

// render.fractal and image.stack: one big picture shared by the whole
// fleet. A job splits the image into horizontal strips (render.fractal
// with part/parts), every device renders the strips it is handed at full
// CPU, and image.stack joins them, top to bottom, into the final PNG.
// Rendering a strip of a whole image (rather than a smaller image) means
// the strips line up exactly: stacking N parts gives the same pixels as
// rendering it in one piece.

// fractalScenes are fixed views of the Mandelbrot set (center and the
// width of the view in the complex plane), so nobody has to type
// coordinates.
var fractalScenes = map[string]struct{ cx, cy, span float64 }{
	"classic":  {-0.5, 0, 3.2},
	"seahorse": {-0.7435669, 0.1314023, 0.0022},
	"spiral":   {-0.761574, -0.0847596, 0.0062},
}

type renderFractal struct{}

func (renderFractal) Available(ctx context.Context) error { return always(ctx) }
func (renderFractal) Run(ctx context.Context, env Env) error {
	w, h := env.intParam("width"), env.intParam("height")
	part, parts, iterations := env.intParam("part"), env.intParam("parts"), env.intParam("iterations")
	scene, ok := fractalScenes[env.param("scene")]
	switch {
	case !ok:
		return fmt.Errorf("unknown scene %q", env.param("scene"))
	case part >= parts:
		return fmt.Errorf("part %d doesn't exist: parts are numbered 0 to %d", part, parts-1)
	case parts > h:
		return fmt.Errorf("%d parts is more than the image's %d rows", parts, h)
	case w*h > catalog.MaxImagePixels:
		return fmt.Errorf("a %dx%d image is over %d million pixels", w, h, catalog.MaxImagePixels/1_000_000)
	}
	y0, y1 := part*h/parts, (part+1)*h/parts
	img := image.NewRGBA(image.Rect(0, 0, w, y1-y0))
	scale := scene.span / float64(w)

	rows := make(chan int)
	var wg sync.WaitGroup
	for i := 0; i < runtime.NumCPU(); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for y := range rows {
				ci := scene.cy + (float64(h)/2-float64(y))*scale
				for x := 0; x < w; x++ {
					cr := scene.cx + (float64(x)-float64(w)/2)*scale
					img.SetRGBA(x, y-y0, fractalColor(cr, ci, iterations))
				}
			}
		}()
	}
	for y := y0; y < y1; y++ {
		select {
		case <-ctx.Done():
			close(rows)
			wg.Wait()
			return ctx.Err()
		case rows <- y:
		}
	}
	close(rows)
	wg.Wait()

	f, err := os.Create(env.out(0))
	if err != nil {
		return err
	}
	if err := png.Encode(f, img); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	_, err = fmt.Fprintf(env.Stdout, "rendered part %d of %d: rows %d-%d of a %dx%d image, %d iterations\n", part+1, parts, y0, y1-1, w, h, iterations)
	return err
}

// fractalColor colors one point by how fast it escapes the Mandelbrot
// set (smooth iteration count through a cosine palette); points inside
// are black.
func fractalColor(cr, ci float64, iterations int) color.RGBA {
	var zr, zi, zr2, zi2 float64
	n := 0
	for ; n < iterations && zr2+zi2 <= 256; n++ {
		zi = 2*zr*zi + ci
		zr = zr2 - zi2 + cr
		zr2, zi2 = zr*zr, zi*zi
	}
	if n >= iterations {
		return color.RGBA{A: 255}
	}
	mu := float64(n) + 1 - math.Log(math.Log(math.Sqrt(zr2+zi2)))/math.Ln2
	t := mu * 0.015
	c := func(phase float64) uint8 {
		return uint8(255 * (0.5 + 0.5*math.Cos(2*math.Pi*(t+phase))))
	}
	return color.RGBA{R: c(0.0), G: c(0.15), B: c(0.30), A: 255}
}

type imageStack struct{}

func (imageStack) Available(ctx context.Context) error { return always(ctx) }
func (imageStack) Run(ctx context.Context, env Env) error {
	// Name order is strip order: a job's parts arrive as
	// parts/<task key>/<name>, and task keys are zero-padded.
	order := make([]int, len(env.Inputs))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return env.Inputs[order[a]] < env.Inputs[order[b]] })

	// Sizes first (headers only): refuse a result too big to hold before
	// allocating anything.
	width, height := 0, 0
	for _, i := range order {
		f, err := os.Open(env.in(i))
		if err != nil {
			return err
		}
		cfg, _, err := image.DecodeConfig(f)
		f.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", env.Inputs[i], err)
		}
		width, height = max(width, cfg.Width), height+cfg.Height
		if width*height > catalog.MaxImagePixels {
			return fmt.Errorf("the stacked image would be over %d million pixels", catalog.MaxImagePixels/1_000_000)
		}
	}
	dst := image.NewRGBA(image.Rect(0, 0, width, height))
	y := 0
	for _, i := range order {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		f, err := os.Open(env.in(i))
		if err != nil {
			return err
		}
		src, _, err := image.Decode(f)
		f.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", env.Inputs[i], err)
		}
		b := src.Bounds()
		draw.Draw(dst, image.Rect(0, y, b.Dx(), y+b.Dy()), src, b.Min, draw.Src)
		y += b.Dy()
	}
	f, err := os.Create(env.out(0))
	if err != nil {
		return err
	}
	if err := png.Encode(f, dst); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	_, err = fmt.Fprintf(env.Stdout, "stacked %d image(s) into %s (%dx%d)\n", len(env.Inputs), env.Outputs[0], width, height)
	return err
}
