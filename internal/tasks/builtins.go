package tasks

import (
	"archive/zip"
	"bufio"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"image"
	_ "image/gif" // registers GIF decoding
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"home-harness/internal/catalog"
)

func always(context.Context) error { return nil }

func (e Env) in(i int) string       { return filepath.Join(e.Dir, filepath.FromSlash(e.Inputs[i])) }
func (e Env) out(i int) string      { return filepath.Join(e.Dir, filepath.FromSlash(e.Outputs[i])) }
func (e Env) param(k string) string { return e.Params[k] }
func (e Env) intParam(k string) int {
	n, _ := strconv.Atoi(e.Params[k]) // validated by catalog.Compile
	return n
}

// ---- system.identity

type identity struct{}

func (identity) Available(ctx context.Context) error { return always(ctx) }
func (identity) Run(ctx context.Context, env Env) error {
	host, _ := os.Hostname()
	enc := json.NewEncoder(env.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(map[string]any{"hostname": host, "os": runtime.GOOS, "arch": runtime.GOARCH, "cpus": runtime.NumCPU()})
}

// ---- cpu.burn

type cpuBurn struct{}

func (cpuBurn) Available(ctx context.Context) error { return always(ctx) }
func (cpuBurn) Run(ctx context.Context, env Env) error {
	threads := env.intParam("threads")
	if threads <= 0 {
		threads = runtime.NumCPU()
	}
	seconds := env.intParam("seconds")
	deadline := time.Now().Add(time.Duration(seconds) * time.Second)
	var total atomic.Uint64
	var wg sync.WaitGroup
	for i := 0; i < threads; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			x := 1.0
			var n uint64
			for time.Now().Before(deadline) && ctx.Err() == nil {
				for j := 0; j < 20000; j++ {
					x = x*1.0000001 + 0.5
					if x > 1e9 {
						x = 1
					}
				}
				n++
			}
			total.Add(n)
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := fmt.Fprintf(env.Stdout, "kept %d thread(s) busy for %ds on %s/%s: %d rounds\n", threads, seconds, runtime.GOOS, runtime.GOARCH, total.Load())
	return err
}

// ---- image.resize

type imageResize struct{}

func (imageResize) Available(ctx context.Context) error { return always(ctx) }
func (imageResize) Run(ctx context.Context, env Env) error {
	f, err := os.Open(env.in(0))
	if err != nil {
		return err
	}
	defer f.Close()
	// Dimensions first: a few-kilobyte file can claim enormous ones.
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return fmt.Errorf("not a readable image: %w", err)
	}
	if int64(cfg.Width)*int64(cfg.Height) > catalog.MaxImagePixels {
		return fmt.Errorf("image is %dx%d, over the %d-pixel limit", cfg.Width, cfg.Height, catalog.MaxImagePixels)
	}
	w, h := env.intParam("width"), env.intParam("height")
	if h == 0 {
		h = int((int64(w)*int64(cfg.Height) + int64(cfg.Width)/2) / int64(cfg.Width))
		if h < 1 {
			h = 1
		}
	}
	if int64(w)*int64(h) > catalog.MaxImagePixels {
		return fmt.Errorf("a %dx%d result is over the %d-pixel limit", w, h, catalog.MaxImagePixels)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	src, _, err := image.Decode(f)
	if err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	dst, err := boxResize(ctx, src, w, h)
	if err != nil {
		return err
	}
	o, err := os.Create(env.out(0))
	if err != nil {
		return err
	}
	switch env.param("format") {
	case "png":
		err = png.Encode(o, dst)
	default:
		err = jpeg.Encode(o, dst, &jpeg.Options{Quality: env.intParam("quality")})
	}
	if cerr := o.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(env.Stdout, "%s: %dx%d -> %dx%d %s\n", env.Inputs[0], cfg.Width, cfg.Height, w, h, env.param("format"))
	return err
}

// boxResize averages the source pixels each destination pixel covers —
// a clean downscale (the common case: photos); an upscale degrades to
// nearest-neighbor.
func boxResize(ctx context.Context, src image.Image, w, h int) (*image.RGBA, error) {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		if y%64 == 0 && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		y0 := b.Min.Y + y*sh/h
		y1 := b.Min.Y + (y+1)*sh/h
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := 0; x < w; x++ {
			x0 := b.Min.X + x*sw/w
			x1 := b.Min.X + (x+1)*sw/w
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var r, g, bl, a, n uint64
			for sy := y0; sy < y1; sy++ {
				for sx := x0; sx < x1; sx++ {
					cr, cg, cb, ca := src.At(sx, sy).RGBA()
					r, g, bl, a, n = r+uint64(cr), g+uint64(cg), bl+uint64(cb), a+uint64(ca), n+1
				}
			}
			i := dst.PixOffset(x, y)
			dst.Pix[i+0] = uint8(r / n >> 8)
			dst.Pix[i+1] = uint8(g / n >> 8)
			dst.Pix[i+2] = uint8(bl / n >> 8)
			dst.Pix[i+3] = uint8(a / n >> 8)
		}
	}
	return dst, nil
}

// ---- archive.zip

type archiveZip struct{}

// Already-compressed formats are stored, not deflated again.
var storedExt = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".zip": true, ".gz": true, ".mp4": true, ".mov": true, ".mp3": true, ".webp": true, ".7z": true}

func (archiveZip) Available(ctx context.Context) error { return always(ctx) }
func (archiveZip) Run(ctx context.Context, env Env) error {
	o, err := os.Create(env.out(0))
	if err != nil {
		return err
	}
	zw := zip.NewWriter(o)
	// Entries go in by base name; a base name already taken keeps its
	// full relative name (a job's parts/0007/...) instead.
	bases := map[string]int{}
	for _, name := range env.Inputs {
		bases[strings.ToLower(path.Base(name))]++
	}
	for i, name := range env.Inputs {
		if ctx.Err() != nil {
			zw.Close()
			o.Close()
			return ctx.Err()
		}
		entry := path.Base(name)
		if bases[strings.ToLower(entry)] > 1 {
			entry = name
		}
		method := zip.Deflate
		if storedExt[strings.ToLower(path.Ext(entry))] {
			method = zip.Store
		}
		w, err := zw.CreateHeader(&zip.FileHeader{Name: entry, Method: method, Modified: time.Now()})
		if err == nil {
			err = copyFile(w, env.in(i))
		}
		if err != nil {
			zw.Close()
			o.Close()
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	err = zw.Close()
	if cerr := o.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(env.Stdout, "packed %d file(s) into %s\n", len(env.Inputs), env.Outputs[0])
	return err
}

func copyFile(w io.Writer, p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(w, f)
	return err
}

// ---- file.hash

type fileHash struct{}

func (fileHash) Available(ctx context.Context) error { return always(ctx) }
func (fileHash) Run(ctx context.Context, env Env) error {
	var lines strings.Builder
	for i, name := range env.Inputs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var h hash.Hash
		switch env.param("algorithm") {
		case "sha1":
			h = sha1.New()
		case "md5":
			h = md5.New()
		default:
			h = sha256.New()
		}
		if err := copyFile(h, env.in(i)); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		fmt.Fprintf(&lines, "%s  %s\n", hex.EncodeToString(h.Sum(nil)), name)
	}
	if err := os.WriteFile(env.out(0), []byte(lines.String()), 0o600); err != nil {
		return err
	}
	_, err := io.WriteString(env.Stdout, lines.String())
	return err
}

// ---- text.count

type textCount struct{}

type counts struct {
	Name  string `json:"name,omitempty"`
	Lines int64  `json:"lines"`
	Words int64  `json:"words"`
	Bytes int64  `json:"bytes"`
}

func (textCount) Available(ctx context.Context) error { return always(ctx) }
func (textCount) Run(ctx context.Context, env Env) error {
	var files []counts
	var total counts
	for i, name := range env.Inputs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		c, err := countFile(env.in(i))
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		c.Name = name
		files = append(files, c)
		total.Lines, total.Words, total.Bytes = total.Lines+c.Lines, total.Words+c.Words, total.Bytes+c.Bytes
	}
	data, err := json.MarshalIndent(map[string]any{"files": files, "total": total}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(env.out(0), append(data, '\n'), 0o600); err != nil {
		return err
	}
	_, err = fmt.Fprintf(env.Stdout, "%d file(s): %d lines, %d words, %d bytes\n", len(files), total.Lines, total.Words, total.Bytes)
	return err
}

func countFile(p string) (counts, error) {
	f, err := os.Open(p)
	if err != nil {
		return counts{}, err
	}
	defer f.Close()
	var c counts
	r := bufio.NewReader(f)
	inWord := false
	for {
		ch, size, err := r.ReadRune()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return counts{}, err
		}
		c.Bytes += int64(size)
		if ch == '\n' {
			c.Lines++
		}
		if unicode.IsSpace(ch) {
			inWord = false
		} else if !inWord {
			inWord = true
			c.Words++
		}
	}
	return c, nil
}
