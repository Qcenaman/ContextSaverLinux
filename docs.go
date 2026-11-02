package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"time"
)

// pdfFileText is a PDF's text with "[page N]" lines between pages. Scanned pages
// (images without text) are read with OCR when o.ocrWait allows it.
func pdfFileText(p string, limit int, o textOpts) (string, error) {
	fi, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	d, err := openPDF(p)
	if err != nil {
		return "", err
	}
	t := readPDFText(d, o.pages, limit)
	if o.pages != nil {
		var bad []string
		for n := range o.pages {
			if n < 1 || n > len(t.pages) {
				bad = append(bad, strconv.Itoa(n))
			}
		}
		if len(bad) == len(o.pages) {
			return "", fmt.Errorf("the PDF has %d page(s)", len(t.pages))
		}
	}
	var scanned []int
	isScanned := map[int]bool{}
	skipped := 0
	for _, n := range t.scanned() {
		if o.pages == nil || o.pages[n] {
			isScanned[n] = true
			if o.ocrUpTo > 0 && n > o.ocrUpTo {
				skipped++
				continue
			}
			scanned = append(scanned, n)
		}
	}
	var ocr map[int]string
	var pending []int
	var oerr error
	if len(scanned) > 0 {
		ocr, pending, oerr = ocrFileCtx(o.ctx, p, fi, scanned, true, o.ocrWait)
	}
	var b strings.Builder
	multi := len(t.pages) > 1
	for i, txt := range t.pages {
		n := i + 1
		if o.pages != nil && !o.pages[n] {
			continue
		}
		if limit > 0 && b.Len() >= limit {
			break
		}
		label := ""
		if isScanned[n] {
			if s, ok := ocr[n]; ok {
				txt, label = s, ", OCR"
				if strings.TrimSpace(s) == "" {
					txt = "[OCR found no text]"
				}
			} else {
				switch {
				case o.ocrUpTo > 0 && n > o.ocrUpTo:
					txt = fmt.Sprintf("[scanned page; read it with pages=%d or grep to OCR it]", n)
				case oerr != nil:
					txt = "[scanned page; OCR failed]"
				case o.ocrWait > 0 && isWin:
					txt = "[scanned page; OCR still running]"
				default:
					txt = "[scanned page without a text layer]"
				}
			}
		}
		if multi {
			fmt.Fprintf(&b, "[page %d%s]\n", n, label)
		} else if label != "" {
			b.WriteString("[OCR text]\n")
		}
		if txt != "" {
			b.WriteString(txt)
			b.WriteByte('\n')
		}
	}
	if skipped > 0 {
		fmt.Fprintf(&b, "[%d more scanned page(s) not OCR'd yet]\n", skipped)
	}
	switch {
	case oerr != nil:
		fmt.Fprintf(&b, "[%v]\n", oerr)
	case len(pending) > 0 && o.ocrWait > 0 && isWin:
		fmt.Fprintf(&b, "[OCR is still reading %d scanned page(s) in the background; ask again in a minute for the rest]\n", len(pending))
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// imageText is the OCR text of a picture.
func imageText(p string, o textOpts) (string, error) {
	fi, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	m, pending, err := ocrFileCtx(o.ctx, p, fi, []int{0}, false, o.ocrWait)
	if t, ok := m[0]; ok {
		if strings.TrimSpace(t) == "" {
			return "[no text found in this image]", nil
		}
		return t, nil
	}
	switch {
	case err != nil:
		return "", err
	case o.ocrWait <= 0:
		return "", errBinary
	case !isWin:
		return "", errors.New("reading text in images needs Windows OCR")
	case len(pending) > 0:
		return "", errors.New("OCR is still reading this image; ask again shortly")
	}
	return "", errBinary
}

func ocrFileCtx(ctx context.Context, p string, fi fs.FileInfo, pages []int, isPDF bool, wait time.Duration) (map[int]string, []int, error) {
	if ctx == nil || wait <= 0 {
		return ocrFile(p, fi, pages, isPDF, wait)
	}
	// stop waiting early when a job is canceled; the OCR itself keeps filling the cache
	type res struct {
		m   map[int]string
		pen []int
		err error
	}
	ch := make(chan res, 1)
	go func() {
		m, pen, err := ocrFile(p, fi, pages, isPDF, wait)
		ch <- res{m, pen, err}
	}()
	select {
	case r := <-ch:
		return r.m, r.pen, r.err
	case <-ctx.Done():
		return ocrFile(p, fi, pages, isPDF, 0)
	}
}

// parsePages reads page lists like "3", "3-5" or "1,4,7-9".
func parsePages(s string) (map[int]bool, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	out := map[int]bool{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		lo, hi, isRange := strings.Cut(part, "-")
		a, err1 := strconv.Atoi(strings.TrimSpace(lo))
		bv, err2 := a, error(nil)
		if isRange {
			bv, err2 = strconv.Atoi(strings.TrimSpace(hi))
		}
		if err1 != nil || err2 != nil || a < 1 || bv < a || bv-a > 5000 {
			return nil, fmt.Errorf("bad pages %q; use e.g. 3, 3-5 or 1,4,7-9", s)
		}
		for n := a; n <= bv; n++ {
			out[n] = true
		}
	}
	return out, nil
}

// pageOfLines maps each line of pdfFileText output to its page number.
func pageOfLines(lines []string) []int {
	out := make([]int, len(lines))
	pg := 1
	for i, l := range lines {
		if f := strings.Fields(strings.TrimPrefix(l, "[page ")); len(f) > 0 && strings.HasPrefix(l, "[page ") {
			if n, err := strconv.Atoi(strings.TrimRight(f[0], "],")); err == nil {
				pg = n
			}
		}
		out[i] = pg
	}
	return out
}

// pdfInfoText describes a PDF: pages, title, and how many pages are scanned.
func pdfInfoText(p string, fi fs.FileInfo) (string, error) {
	s := fileInfoText(p, fi)
	d, err := openPDF(p)
	if err != nil {
		return s + ", PDF: " + err.Error(), nil
	}
	t := readPDFText(d, nil, 50<<20)
	chars, withText := 0, 0
	for _, pg := range t.pages {
		chars += len([]rune(pg))
		if len(strings.TrimSpace(pg)) >= 3 {
			withText++
		}
	}
	s += fmt.Sprintf(", PDF with %d page(s), %s characters of text", len(t.pages), hc(int64(chars)))
	if t.title != "" {
		s += fmt.Sprintf(", title %q", clip(t.title, 80))
	}
	if sc := t.scanned(); len(sc) > 0 {
		have, _, _ := ocrFile(p, fi, sc, true, 0)
		s += fmt.Sprintf(", %d scanned page(s) (%d already read by OCR)", len(sc), len(have))
	}
	return s, nil
}
