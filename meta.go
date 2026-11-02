package main

import (
	"bufio"
	"encoding/binary"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var takenCache sync.Map

// dateTaken returns when a photo or video was taken, falling back to the modified date.
func dateTaken(p string, info fs.FileInfo) time.Time {
	if v, ok := takenCache.Load(p); ok {
		return v.(time.Time)
	}
	var t time.Time
	switch strings.ToLower(filepath.Ext(p)) {
	case ".jpg", ".jpeg":
		t = exifJPEG(p)
	case ".mp4", ".mov", ".m4v", ".3gp":
		t = mp4Created(p)
	}
	if t.IsZero() || t.Year() < 1971 {
		t = info.ModTime()
	}
	takenCache.Store(p, t)
	return t
}

func exifJPEG(p string) time.Time {
	f, err := os.Open(p)
	if err != nil {
		return time.Time{}
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64<<10)
	var hdr [2]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil || hdr != [2]byte{0xFF, 0xD8} {
		return time.Time{}
	}
	for i := 0; i < 40; i++ {
		var m [4]byte
		if _, err := io.ReadFull(r, m[:]); err != nil || m[0] != 0xFF {
			return time.Time{}
		}
		if m[1] == 0xDA || m[1] == 0xD9 {
			return time.Time{}
		}
		ln := int(binary.BigEndian.Uint16(m[2:])) - 2
		if ln < 0 {
			return time.Time{}
		}
		if m[1] == 0xE1 && ln > 14 {
			seg := make([]byte, ln)
			if _, err := io.ReadFull(r, seg); err != nil {
				return time.Time{}
			}
			if string(seg[:6]) == "Exif\x00\x00" {
				return tiffDate(seg[6:])
			}
			continue
		}
		if _, err := r.Discard(ln); err != nil {
			return time.Time{}
		}
	}
	return time.Time{}
}

func tiffDate(b []byte) time.Time {
	if len(b) < 8 {
		return time.Time{}
	}
	var bo binary.ByteOrder
	switch string(b[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return time.Time{}
	}
	find := func(off int, tag uint16) (uint32, uint32, bool) {
		if off < 0 || off+2 > len(b) {
			return 0, 0, false
		}
		n := int(bo.Uint16(b[off:]))
		for i := 0; i < n; i++ {
			e := off + 2 + i*12
			if e+12 > len(b) {
				return 0, 0, false
			}
			if bo.Uint16(b[e:]) == tag {
				return bo.Uint32(b[e+4:]), bo.Uint32(b[e+8:]), true
			}
		}
		return 0, 0, false
	}
	str := func(cnt, off uint32) string {
		if cnt < 2 || int(off)+int(cnt) > len(b) {
			return ""
		}
		return strings.TrimRight(string(b[off:off+cnt-1]), "\x00 ")
	}
	parse := func(s string) time.Time {
		t, err := time.ParseInLocation("2006:01:02 15:04:05", s, time.Local)
		if err != nil {
			return time.Time{}
		}
		return t
	}
	ifd0 := int(bo.Uint32(b[4:8]))
	if _, exifOff, ok := find(ifd0, 0x8769); ok {
		if cnt, off, ok := find(int(exifOff), 0x9003); ok {
			if t := parse(str(cnt, off)); !t.IsZero() {
				return t
			}
		}
	}
	if cnt, off, ok := find(ifd0, 0x0132); ok {
		return parse(str(cnt, off))
	}
	return time.Time{}
}

func mp4Created(p string) time.Time {
	f, err := os.Open(p)
	if err != nil {
		return time.Time{}
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return time.Time{}
	}
	size := st.Size()
	box := func(off int64) (int64, string, int64, bool) {
		var h [16]byte
		if _, err := f.ReadAt(h[:8], off); err != nil {
			return 0, "", 0, false
		}
		sz := int64(binary.BigEndian.Uint32(h[:4]))
		typ := string(h[4:8])
		hl := int64(8)
		if sz == 1 {
			if _, err := f.ReadAt(h[8:16], off+8); err != nil {
				return 0, "", 0, false
			}
			sz = int64(binary.BigEndian.Uint64(h[8:16]))
			hl = 16
		} else if sz == 0 {
			sz = size - off
		}
		if sz < hl {
			return 0, "", 0, false
		}
		return sz, typ, hl, true
	}
	for off, i := int64(0), 0; off+8 <= size && i < 64; i++ {
		sz, typ, hl, ok := box(off)
		if !ok {
			return time.Time{}
		}
		if typ == "moov" {
			end := off + sz
			for c := off + hl; c+8 <= end; {
				csz, ctyp, _, ok := box(c)
				if !ok {
					return time.Time{}
				}
				if ctyp == "mvhd" {
					var v [1]byte
					f.ReadAt(v[:], c+8)
					var secs uint64
					if v[0] == 1 {
						var x [8]byte
						f.ReadAt(x[:], c+12)
						secs = binary.BigEndian.Uint64(x[:])
					} else {
						var x [4]byte
						f.ReadAt(x[:], c+12)
						secs = uint64(binary.BigEndian.Uint32(x[:]))
					}
					if secs < 2082844800 {
						return time.Time{}
					}
					return time.Unix(int64(secs)-2082844800, 0).Local()
				}
				c += csz
			}
			return time.Time{}
		}
		off += sz
	}
	return time.Time{}
}

var categories = map[string]string{}

func init() {
	add := func(cat string, exts string) {
		for _, e := range strings.Fields(exts) {
			categories["."+e] = cat
		}
	}
	add("Images", "jpg jpeg png gif bmp webp heic heif tif tiff raw cr2 cr3 nef arw dng orf rw2 svg ico psd")
	add("Videos", "mp4 mov avi mkv wmv m4v webm 3gp flv mts m2ts mpg mpeg")
	add("Audio", "mp3 wav flac aac m4a ogg wma opus aiff")
	add("Documents", "pdf doc docx xls xlsx ppt pptx txt rtf odt ods odp csv md epub")
	add("Archives", "zip rar 7z tar gz bz2 xz iso")
	add("Programs", "exe msi bat cmd ps1 apk appx msix")
	add("Code", "go py js ts html htm css json xml java c cpp h cs php rb rs sh sql yaml yml")
}

func category(ext string) string {
	if c, ok := categories[strings.ToLower(ext)]; ok {
		return c
	}
	return "Other"
}
