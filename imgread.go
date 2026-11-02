package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var imageExts = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".jfif": true, ".gif": true,
	".bmp": true, ".tif": true, ".tiff": true, ".webp": true, ".heic": true, ".heif": true}

func isImageExt(ext string) bool { return imageExts[strings.ToLower(ext)] }

func isJPEGExt(ext string) bool {
	switch strings.ToLower(ext) {
	case ".jpg", ".jpeg", ".jfif":
		return true
	}
	return false
}

// jpegOrientation is the EXIF orientation (1-8) of a JPEG; 1 when there is none.
func jpegOrientation(b []byte) int {
	if len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 {
		return 1
	}
	for p := 2; p+4 <= len(b); {
		if b[p] != 0xFF {
			return 1
		}
		m := b[p+1]
		if m == 0xDA || m == 0xD9 {
			return 1
		}
		ln := int(binary.BigEndian.Uint16(b[p+2:]))
		if ln < 2 || p+2+ln > len(b) {
			return 1
		}
		if seg := b[p+4 : p+2+ln]; m == 0xE1 && len(seg) > 14 && string(seg[:6]) == "Exif\x00\x00" {
			return tiffOrientation(seg[6:])
		}
		p += 2 + ln
	}
	return 1
}

func tiffOrientation(t []byte) int {
	if len(t) < 8 {
		return 1
	}
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 1
	}
	off := int(bo.Uint32(t[4:]))
	if off < 0 || off+2 > len(t) {
		return 1
	}
	for i := 0; i < int(bo.Uint16(t[off:])); i++ {
		e := off + 2 + 12*i
		if e+12 > len(t) {
			return 1
		}
		if bo.Uint16(t[e:]) == 0x0112 {
			if v := int(bo.Uint16(t[e+8:])); v >= 1 && v <= 8 {
				return v
			}
			return 1
		}
	}
	return 1
}

// flatten draws an image onto white, so transparent parts don't turn black in a JPEG.
func flatten(src image.Image) *image.RGBA {
	b := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(dst, dst.Bounds(), &image.Uniform{color.White}, image.Point{}, draw.Src)
	draw.Draw(dst, dst.Bounds(), src, b.Min, draw.Over)
	return dst
}

// shrink box-filters an image so its long side is at most maxPx.
func shrink(src *image.RGBA, maxPx int) *image.RGBA {
	w, h := src.Bounds().Dx(), src.Bounds().Dy()
	if maxPx <= 0 || w <= maxPx && h <= maxPx {
		return src
	}
	s := float64(maxPx) / float64(max(w, h))
	nw, nh := max(1, int(float64(w)*s+0.5)), max(1, int(float64(h)*s+0.5))
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	for y := 0; y < nh; y++ {
		y0, y1 := y*h/nh, max((y+1)*h/nh, y*h/nh+1)
		for x := 0; x < nw; x++ {
			x0, x1 := x*w/nw, max((x+1)*w/nw, x*w/nw+1)
			var r, g, bl, a, n uint64
			for yy := y0; yy < y1; yy++ {
				row := src.Pix[yy*src.Stride:]
				for xx := x0; xx < x1; xx++ {
					p := row[xx*4 : xx*4+4]
					r, g, bl, a, n = r+uint64(p[0]), g+uint64(p[1]), bl+uint64(p[2]), a+uint64(p[3]), n+1
				}
			}
			q := dst.Pix[y*dst.Stride+x*4:]
			q[0], q[1], q[2], q[3] = uint8(r/n), uint8(g/n), uint8(bl/n), uint8(a/n)
		}
	}
	return dst
}

// orient turns an image upright from its EXIF orientation.
func orient(src *image.RGBA, o int) *image.RGBA {
	if o <= 1 || o > 8 {
		return src
	}
	w, h := src.Bounds().Dx(), src.Bounds().Dy()
	dw, dh := w, h
	if o >= 5 {
		dw, dh = h, w
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			nx, ny := x, y
			switch o {
			case 2:
				nx = w - 1 - x
			case 3:
				nx, ny = w-1-x, h-1-y
			case 4:
				ny = h - 1 - y
			case 5:
				nx, ny = y, x
			case 6:
				nx, ny = h-1-y, x
			case 7:
				nx, ny = h-1-y, w-1-x
			case 8:
				nx, ny = y, w-1-x
			}
			copy(dst.Pix[ny*dst.Stride+nx*4:ny*dst.Stride+nx*4+4], src.Pix[y*src.Stride+x*4:y*src.Stride+x*4+4])
		}
	}
	return dst
}

func encodeJPEG(img image.Image) []byte {
	var out bytes.Buffer
	jpeg.Encode(&out, img, &jpeg.Options{Quality: 85})
	return out.Bytes()
}

func jpegEncodeQ(w io.Writer, img image.Image, q int) error {
	return jpeg.Encode(w, img, &jpeg.Options{Quality: q})
}

type preview struct {
	jpg          []byte
	ow, oh, w, h int // original and copy sizes
}

// imagePreview makes a JPEG copy of an image file whose long side is at most maxPx.
func imagePreview(p string, maxPx int) (*preview, error) {
	ext := strings.ToLower(filepath.Ext(p))
	switch ext {
	case ".png", ".jpg", ".jpeg", ".jfif", ".gif":
		fi, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if fi.Size() > 300<<20 {
			return nil, errors.New("image is over 300 MB")
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		if cfg, _, err := image.DecodeConfig(bytes.NewReader(b)); err == nil && cfg.Width*cfg.Height > 200_000_000 {
			return nil, errors.New("image is too large to decode")
		}
		if img, _, err := image.Decode(bytes.NewReader(b)); err == nil {
			full := flatten(img)
			o := 1
			if isJPEGExt(ext) {
				o = jpegOrientation(b)
			}
			small := orient(shrink(full, maxPx), o)
			pv := &preview{jpg: encodeJPEG(small), ow: full.Bounds().Dx(), oh: full.Bounds().Dy(), w: small.Bounds().Dx(), h: small.Bounds().Dy()}
			if o >= 5 {
				pv.ow, pv.oh = pv.oh, pv.ow
			}
			return pv, nil
		}
	}
	if !isWin {
		return nil, fmt.Errorf("can't open %s images here", ext)
	}
	var pg ocrPage
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	jpg, err := runWinOCR(ctx, ocrReq{mode: "convert", path: p, maxPx: maxPx}, func(x ocrPage) { pg = x })
	if err != nil {
		return nil, err
	}
	return jpegPreview(jpg, pg.W, pg.H, maxPx)
}

// jpegPreview wraps a JPEG made by Windows, shrinking it when display scaling made it bigger than asked.
func jpegPreview(jpg []byte, ow, oh, maxPx int) (*preview, error) {
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(jpg))
	if err != nil {
		return nil, errors.New("couldn't make a copy of the image")
	}
	if ow == 0 {
		ow, oh = cfg.Width, cfg.Height
	}
	if maxPx > 0 && max(cfg.Width, cfg.Height) > maxPx {
		if img, err := jpeg.Decode(bytes.NewReader(jpg)); err == nil {
			small := shrink(flatten(img), maxPx)
			return &preview{jpg: encodeJPEG(small), ow: ow, oh: oh, w: small.Bounds().Dx(), h: small.Bounds().Dy()}, nil
		}
	}
	return &preview{jpg: jpg, ow: ow, oh: oh, w: cfg.Width, h: cfg.Height}, nil
}

// pdfPagePreview renders one PDF page with Windows' PDF renderer.
func pdfPagePreview(p string, page, maxPx int) (*preview, error) {
	if !isWin {
		return nil, errors.New("showing PDF pages needs Windows")
	}
	var pg ocrPage
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	jpg, err := runWinOCR(ctx, ocrReq{mode: "pdfimage", path: p, pages: []int{page}, maxPx: maxPx}, func(x ocrPage) { pg = x })
	if err != nil {
		return nil, err
	}
	if len(jpg) == 0 {
		return nil, fmt.Errorf("the PDF has no page %d", page)
	}
	return jpegPreview(jpg, pg.W, pg.H, maxPx)
}

func imageInfoText(p string, fi fs.FileInfo) string {
	s := fileInfoText(p, fi)
	f, err := os.Open(p)
	if err != nil {
		return s
	}
	defer f.Close()
	if cfg, format, err := image.DecodeConfig(f); err == nil {
		w, h := cfg.Width, cfg.Height
		if isJPEGExt(filepath.Ext(p)) {
			head := make([]byte, 256<<10)
			f.Seek(0, 0)
			n, _ := f.Read(head)
			if o := jpegOrientation(head[:n]); o >= 5 {
				w, h = h, w
			}
		}
		s += fmt.Sprintf(", %s image %d×%d", format, w, h)
	}
	if isJPEGExt(filepath.Ext(p)) {
		if t := exifJPEG(p); !t.IsZero() {
			s += ", taken " + t.Format("2006-01-02 15:04")
		}
	}
	return s
}
