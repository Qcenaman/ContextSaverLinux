package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// OCR, picture conversion and PDF page images on Linux, behind the same interface as the
// Windows script: tesseract reads text with word boxes, pdftoppm renders PDF pages, and
// ImageMagick (or ffmpeg) opens picture formats Go can't.

func ocrAvailable() bool {
	_, err := exec.LookPath("tesseract")
	return err == nil
}

func runLinuxOCR(ctx context.Context, r ocrReq, each func(ocrPage)) ([]byte, error) {
	tmp, err := os.MkdirTemp("", "contextsaver-ocr")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	switch r.mode {
	case "convert":
		return convertPicture(ctx, r.path, tmp, r.maxPx, each)
	case "pdfimage":
		if len(r.pages) == 0 {
			return nil, errors.New("no page given")
		}
		png, err := pdfPagePNG(ctx, r.path, r.pages[0], tmp, 150, r.maxPx, "-jpeg")
		if err != nil {
			return nil, err
		}
		b, _ := os.ReadFile(png)
		if cfg, _, err := image.DecodeConfig(bytes.NewReader(b)); err == nil && each != nil {
			each(ocrPage{N: r.pages[0], W: cfg.Width, H: cfg.Height})
		}
		return b, nil
	}
	if !ocrAvailable() {
		return nil, errors.New("reading text in pictures needs tesseract (" + installHint("tesseract-ocr") + ")")
	}
	switch r.mode {
	case "image", "stdin":
		p := r.path
		if r.mode == "stdin" {
			p = filepath.Join(tmp, "in.png")
			if err := os.WriteFile(p, r.stdin, 0o600); err != nil {
				return nil, err
			}
		}
		pg, err := tesseractPage(ctx, p, tmp, r.upscale)
		if err != nil {
			return nil, err
		}
		if each != nil {
			each(pg)
		}
		return nil, nil
	case "pdf":
		pages := r.pages
		if len(pages) == 0 {
			n := pdfPageCount(ctx, r.path)
			for i := 1; i <= n; i++ {
				pages = append(pages, i)
			}
		}
		for _, n := range pages {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			png, err := pdfPagePNG(ctx, r.path, n, tmp, 300, 0, "-png")
			if err != nil {
				return nil, err
			}
			pg, err := tesseractPage(ctx, png, tmp, false)
			if err != nil {
				return nil, err
			}
			pg.N = n
			if each != nil {
				each(pg)
			}
			os.Remove(png)
		}
		return nil, nil
	}
	return nil, fmt.Errorf("unknown OCR mode %q", r.mode)
}

// tesseractPage reads one picture into lines of words with their boxes (TSV output).
// upscale doubles small screenshots first; boxes are mapped back to the original size.
func tesseractPage(ctx context.Context, p, tmp string, upscale bool) (ocrPage, error) {
	var pg ocrPage
	if f, err := os.Open(p); err == nil {
		if cfg, _, err := image.DecodeConfig(f); err == nil {
			pg.W, pg.H = cfg.Width, cfg.Height
		}
		f.Close()
	}
	src, scale := p, 1
	if upscale && pg.W > 0 && pg.W < 2000 {
		if _, err := exec.LookPath("convert"); err == nil {
			big := filepath.Join(tmp, "up.png")
			if exec.CommandContext(ctx, "convert", p, "-resize", "200%", big).Run() == nil {
				src, scale = big, 2
			}
		}
	}
	out, err := exec.CommandContext(ctx, "tesseract", src, "stdout", "--psm", "3", "tsv").Output()
	if err != nil {
		if ctx.Err() != nil {
			return pg, ctx.Err()
		}
		return pg, fmt.Errorf("tesseract couldn't read %s: %v", filepath.Base(p), err)
	}
	type key struct{ block, par, line int }
	lines := map[key]*ocrLine{}
	var order []key
	for i, l := range strings.Split(string(out), "\n") {
		f := strings.Split(l, "\t")
		if i == 0 || len(f) < 12 || f[0] != "5" || strings.TrimSpace(f[11]) == "" {
			continue
		}
		num := func(k int) int { n, _ := strconv.Atoi(f[k]); return n }
		k := key{num(2), num(3), num(4)}
		ln := lines[k]
		if ln == nil {
			ln = &ocrLine{}
			lines[k] = ln
			order = append(order, k)
		}
		ln.Words = append(ln.Words, ocrWord{X: num(6) / scale, Y: num(7) / scale, W: num(8) / scale, H: num(9) / scale, Text: f[11]})
	}
	sort.SliceStable(order, func(i, j int) bool {
		a, b := lines[order[i]].Words[0], lines[order[j]].Words[0]
		if abs(a.Y-b.Y) > max(a.H, b.H)/2 {
			return a.Y < b.Y
		}
		return a.X < b.X
	})
	for _, k := range order {
		ln := lines[k]
		ws := make([]string, len(ln.Words))
		for i, w := range ln.Words {
			ws[i] = w.Text
		}
		ln.Text = strings.Join(ws, " ")
		pg.Lines = append(pg.Lines, *ln)
	}
	return pg, nil
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// pdfPagePNG renders page n of a PDF with pdftoppm (format -png or -jpeg).
func pdfPagePNG(ctx context.Context, pdf string, n int, tmp string, dpi, maxPx int, format string) (string, error) {
	if _, err := exec.LookPath("pdftoppm"); err != nil {
		return "", errors.New("PDF pages need pdftoppm (" + installHint("poppler-utils") + ")")
	}
	base := filepath.Join(tmp, "p"+strconv.Itoa(n))
	args := []string{format, "-f", strconv.Itoa(n), "-l", strconv.Itoa(n), "-singlefile"}
	if maxPx > 0 {
		args = append(args, "-scale-to", strconv.Itoa(maxPx))
	} else {
		args = append(args, "-r", strconv.Itoa(dpi))
	}
	if out, err := exec.CommandContext(ctx, "pdftoppm", append(args, pdf, base)...).CombinedOutput(); err != nil {
		return "", fmt.Errorf("pdftoppm: %s", strings.TrimSpace(string(out)))
	}
	ext := ".png"
	if format == "-jpeg" {
		ext = ".jpg"
	}
	if !exists(base + ext) {
		return "", fmt.Errorf("the PDF has no page %d", n)
	}
	return base + ext, nil
}

func pdfPageCount(ctx context.Context, pdf string) int {
	out, err := exec.CommandContext(ctx, "pdfinfo", pdf).Output()
	if err != nil {
		return 1
	}
	for _, l := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(l, "Pages:"); ok {
			n, _ := strconv.Atoi(strings.TrimSpace(v))
			return max(n, 1)
		}
	}
	return 1
}

// convertPicture makes a JPEG of a picture Go can't decode (HEIC, WebP, TIFF, BMP...).
func convertPicture(ctx context.Context, p, tmp string, maxPx int, each func(ocrPage)) ([]byte, error) {
	dst := filepath.Join(tmp, "out.jpg")
	size := ""
	if maxPx > 0 {
		size = fmt.Sprintf("%dx%d>", maxPx, maxPx)
	}
	var err error
	switch {
	case lookOK("magick"):
		args := []string{p + "[0]", "-auto-orient"}
		if size != "" {
			args = append(args, "-resize", size)
		}
		err = exec.CommandContext(ctx, "magick", append(args, dst)...).Run()
	case lookOK("convert"):
		args := []string{p + "[0]", "-auto-orient"}
		if size != "" {
			args = append(args, "-resize", size)
		}
		err = exec.CommandContext(ctx, "convert", append(args, dst)...).Run()
	case lookOK("ffmpeg"):
		args := []string{"-y", "-loglevel", "error", "-i", p, "-frames:v", "1"}
		if maxPx > 0 {
			args = append(args, "-vf", fmt.Sprintf("scale='min(%d,iw)':-2", maxPx))
		}
		err = exec.CommandContext(ctx, "ffmpeg", append(args, dst)...).Run()
	default:
		return nil, fmt.Errorf("can't open %s pictures here (%s)", filepath.Ext(p), installHint("imagemagick"))
	}
	if err != nil {
		return nil, fmt.Errorf("couldn't convert %s: %v", filepath.Base(p), err)
	}
	b, err := os.ReadFile(dst)
	if err != nil {
		return nil, err
	}
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(b)); err == nil && each != nil {
		each(ocrPage{W: cfg.Width, H: cfg.Height})
	}
	return b, nil
}

func lookOK(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// installHint: the apt command for a missing program.
func installHint(pkg string) string { return "sudo apt install " + pkg + ", or contextsaver setup" }
