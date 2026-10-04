package tasks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"home-harness/internal/catalog"
)

// ---- doc.text (poppler's pdftotext and pdftoppm, tesseract)

// OCR limits: pages are rendered at most 3500 pixels on their long side
// (about 300 dpi for A4, what tesseract wants), and at most this many.
const (
	ocrPageSide = 3500
	ocrMaxPages = 100
	// A PDF with less text than this per page has no real text layer
	// (a scan): auto recognizes its pages instead.
	minTextPerPage = 16
)

type docTools struct{ pdftotext, pdftoppm, tesseract string }

func (t *tools) docTools() docTools {
	return docTools{pdftotext: t.find("pdftotext"), pdftoppm: t.find("pdftoppm"), tesseract: t.find("tesseract")}
}

// methods are the doc.text methods a device with these programs can run:
// text needs pdftotext; ocr tesseract (a PDF's pages also pdftoppm, which
// comes with pdftotext); auto everything, since it may need any of them.
func (f docTools) methods() []string {
	var m []string
	if f.pdftotext != "" && f.pdftoppm != "" && f.tesseract != "" {
		m = append(m, "auto")
	}
	if f.tesseract != "" {
		m = append(m, "ocr")
	}
	if f.pdftotext != "" {
		m = append(m, "text")
	}
	return m
}

func (f docTools) names() []string {
	var n []string
	for _, p := range []struct{ name, path string }{{"pdftoppm", f.pdftoppm}, {"pdftotext", f.pdftotext}, {"tesseract", f.tesseract}} {
		if p.path != "" {
			n = append(n, p.name)
		}
	}
	return n
}

type docText struct{ t *tools }

func (d docText) Available(context.Context) error {
	if f := d.t.docTools(); f.pdftotext == "" && f.tesseract == "" {
		return errors.New("neither pdftotext (poppler) nor tesseract is installed (-tools-dir, or a standard place)")
	}
	return nil
}

func (d docText) Attributes(context.Context) map[string]string {
	f := d.t.docTools()
	return map[string]string{catalog.AttrDocMethods: strings.Join(f.methods(), ","), catalog.AttrDocTools: strings.Join(f.names(), ",")}
}

// pdftotextArgs: the PDF's text as UTF-8, pages ending in a form feed.
func pdftotextArgs(in, out string) []string { return []string{"-enc", "UTF-8", in, out} }

// pdftoppmArgs: grayscale PNG pages <prefix>-<n>.png.
func pdftoppmArgs(in, prefix string) []string {
	return []string{"-png", "-gray", "-scale-to", strconv.Itoa(ocrPageSide), "-l", strconv.Itoa(ocrMaxPages), in, prefix}
}

// tesseractArgs: the picture's text in <base>.txt.
func tesseractArgs(img, base, language string) []string { return []string{img, base, "-l", language} }

func (d docText) Run(ctx context.Context, env Env) error {
	f := d.t.docTools()
	method := env.param("method")
	tmp, err := tempDir(env)
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	in := env.in(0)
	var text, how string
	if hasExt(env.Inputs[0], "pdf") {
		pages := 0
		if method != "ocr" {
			if f.pdftotext == "" {
				return errors.New("this device has no pdftotext (poppler) to read a PDF's text")
			}
			out := filepath.Join(tmp, "pdf.txt")
			if err := d.t.exec(ctx, env, f.pdftotext, pdftotextArgs(in, out)...); err != nil {
				return err
			}
			b, err := os.ReadFile(out)
			if err != nil {
				return errors.New("pdftotext wrote nothing")
			}
			pages = max(1, bytes.Count(b, []byte("\f")))
			if method == "text" || textChars(b) >= minTextPerPage*pages {
				text, how = string(b), fmt.Sprintf("the text of %d page(s)", pages)
			} else {
				fmt.Fprintf(env.Stdout, "%s has no text layer (a scan?): recognizing its pages\n", env.Inputs[0])
			}
		}
		if how == "" {
			if f.pdftoppm == "" || f.tesseract == "" {
				return errors.New("recognizing a PDF's pages needs poppler's pdftoppm and tesseract on the device")
			}
			text, how, err = d.ocrPDF(ctx, env, f, in, tmp, pages)
			if err != nil {
				return err
			}
		}
	} else {
		if method == "text" {
			return errors.New("a picture has no text layer: use ocr or auto")
		}
		if f.tesseract == "" {
			return errors.New("this device has no tesseract to recognize text")
		}
		if err := checkImagePixels(in); err != nil {
			return err
		}
		if text, err = d.ocr(ctx, env, f, in, filepath.Join(tmp, "ocr")); err != nil {
			return err
		}
		how = "recognized text"
	}
	if err := os.WriteFile(env.out(0), []byte(text), 0o600); err != nil {
		return err
	}
	_, err = fmt.Fprintf(env.Stdout, "%s: %d characters, %s\n", env.Outputs[0], len([]rune(text)), how)
	return err
}

// ocrPDF renders a PDF's pages and recognizes each; pages is how many
// pdftotext counted (0 if it didn't run).
func (d docText) ocrPDF(ctx context.Context, env Env, f docTools, in, tmp string, pages int) (string, string, error) {
	prefix := filepath.Join(tmp, "page")
	if err := d.t.exec(ctx, env, f.pdftoppm, pdftoppmArgs(in, prefix)...); err != nil {
		return "", "", err
	}
	images := pageImages(tmp)
	if len(images) == 0 {
		return "", "", errors.New("pdftoppm made no page images")
	}
	parts := make([]string, len(images))
	for i, img := range images {
		text, err := d.ocr(ctx, env, f, img, filepath.Join(tmp, fmt.Sprintf("ocr-%04d", i+1)))
		if err != nil {
			return "", "", fmt.Errorf("page %d: %w", i+1, err)
		}
		parts[i] = text
	}
	how := fmt.Sprintf("recognized on %d page(s)", len(images))
	if pages > len(images) {
		how += fmt.Sprintf(" (the first %d of %d)", len(images), pages)
	}
	// Pages end in a form feed, as pdftotext's do.
	return strings.Join(parts, "\f") + "\f", how, nil
}

// pageImages are pdftoppm's <dir>/page-<n>.png in page order (it pads n
// to the page count's width, so name order isn't enough).
func pageImages(dir string) []string {
	entries, _ := os.ReadDir(dir)
	type page struct {
		n    int
		path string
	}
	var pages []page
	for _, e := range entries {
		num, ok := strings.CutPrefix(e.Name(), "page-")
		num, ok2 := strings.CutSuffix(num, ".png")
		n, err := strconv.Atoi(num)
		if ok && ok2 && err == nil {
			pages = append(pages, page{n, filepath.Join(dir, e.Name())})
		}
	}
	sort.Slice(pages, func(i, j int) bool { return pages[i].n < pages[j].n })
	out := make([]string, len(pages))
	for i, p := range pages {
		out[i] = p.path
	}
	return out
}

// ocr has tesseract read one picture.
func (d docText) ocr(ctx context.Context, env Env, f docTools, img, base string) (string, error) {
	if err := d.t.exec(ctx, env, f.tesseract, tesseractArgs(img, base, env.param("language"))...); err != nil {
		return "", err
	}
	b, err := os.ReadFile(base + ".txt")
	if err != nil {
		return "", errors.New("tesseract wrote nothing")
	}
	return string(b), nil
}

// textChars counts the characters that aren't spaces or page breaks.
func textChars(b []byte) int {
	n := 0
	for _, r := range string(b) {
		if !unicode.IsSpace(r) {
			n++
		}
	}
	return n
}

// checkImagePixels refuses a picture over catalog.MaxImagePixels from its
// header alone, before a program decodes it (a decompression bomb).
func checkImagePixels(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return fmt.Errorf("not a readable picture: %w", err)
	}
	if int64(cfg.Width)*int64(cfg.Height) > catalog.MaxImagePixels {
		return fmt.Errorf("picture is %dx%d, over the %d-pixel limit", cfg.Width, cfg.Height, catalog.MaxImagePixels)
	}
	return nil
}
