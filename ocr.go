package main

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// OCR and PDF page rendering with what Windows already has: Windows.Media.Ocr and
// Windows.Data.Pdf, reached through Windows PowerShell 5.1, which can call WinRT.
// Nothing to install. Results are cached per file, so a document is only read once.

const winOCRScript = `param([string]$Mode = 'image', [string]$Path = '', [string]$Pages = '', [int]$Width = 0, [int]$Boxes = 0, [int]$Upscale = 0, [int]$MaxPx = 0, [string]$Lang = '')
$ErrorActionPreference = 'Stop'
$out = New-Object System.IO.StreamWriter([Console]::OpenStandardOutput(), (New-Object System.Text.UTF8Encoding $false))
function Say([string]$s) { $out.WriteLine($s); $out.Flush() }
function Await($op, [Type]$t) { $task = $script:opT.MakeGenericMethod($t).Invoke($null, @($op)); [void]$task.Wait(-1); $task.Result }
function AwaitAct($op) { $task = $script:actT.Invoke($null, @($op)); [void]$task.Wait(-1) }
function Open-Decoder([string]$p) {
  $file = Await ([Windows.Storage.StorageFile]::GetFileFromPathAsync($p)) ([Windows.Storage.StorageFile])
  $st = Await ($file.OpenAsync([Windows.Storage.FileAccessMode]::Read)) ([Windows.Storage.Streams.IRandomAccessStream])
  Await ([Windows.Graphics.Imaging.BitmapDecoder]::CreateAsync($st)) ([Windows.Graphics.Imaging.BitmapDecoder])
}
function Get-Bitmap($dec, [double]$s, $alpha) {
  $t = New-Object Windows.Graphics.Imaging.BitmapTransform
  if ($s -ne 1.0) {
    $t.ScaledWidth = [uint32][Math]::Max(1, [Math]::Round($dec.PixelWidth * $s))
    $t.ScaledHeight = [uint32][Math]::Max(1, [Math]::Round($dec.PixelHeight * $s))
    $t.InterpolationMode = [Windows.Graphics.Imaging.BitmapInterpolationMode]::Fant
  }
  Await ($dec.GetSoftwareBitmapAsync([Windows.Graphics.Imaging.BitmapPixelFormat]::Bgra8, $alpha, $t, [Windows.Graphics.Imaging.ExifOrientationMode]::RespectExifOrientation, [Windows.Graphics.Imaging.ColorManagementMode]::ColorManageToSRgb)) ([Windows.Graphics.Imaging.SoftwareBitmap])
}
function Ocr-Decoder($dec, $tag) {
  $w = [double]$dec.PixelWidth; $h = [double]$dec.PixelHeight
  $s = 1.0
  if ($Upscale -eq 1 -and $w -lt 1600 -and $h -lt 1600) { $s = 2.0 }
  if ($w * $s -gt $script:max -or $h * $s -gt $script:max) { $s = [Math]::Min($script:max / $w, $script:max / $h) }
  $bmp = Get-Bitmap $dec $s ([Windows.Graphics.Imaging.BitmapAlphaMode]::Premultiplied)
  $res = Await ($script:engine.RecognizeAsync($bmp)) ([Windows.Media.Ocr.OcrResult])
  Say ('P {0} {1} {2}' -f $tag, $dec.OrientedPixelWidth, $dec.OrientedPixelHeight)
  foreach ($ln in $res.Lines) {
    Say ('L ' + $ln.Text)
    if ($Boxes -eq 1) {
      foreach ($wd in $ln.Words) {
        $b = $wd.BoundingRect
        Say ('W {0} {1} {2} {3} {4}' -f [int]($b.X / $s), [int]($b.Y / $s), [int]($b.Width / $s), [int]($b.Height / $s), $wd.Text)
      }
    }
  }
}
function Open-Pdf([string]$p) {
  $file = Await ([Windows.Storage.StorageFile]::GetFileFromPathAsync($p)) ([Windows.Storage.StorageFile])
  Await ([Windows.Data.Pdf.PdfDocument]::LoadFromFileAsync($file)) ([Windows.Data.Pdf.PdfDocument])
}
function Say-Jpeg($st) {
  $src = [System.IO.WindowsRuntimeStreamExtensions]::AsStreamForRead($st.GetInputStreamAt(0))
  $mem = New-Object System.IO.MemoryStream
  $src.CopyTo($mem)
  Say ('J ' + [Convert]::ToBase64String($mem.ToArray()))
}
function Convert-Decoder($dec) {
  $bmp = Get-Bitmap $dec 1.0 ([Windows.Graphics.Imaging.BitmapAlphaMode]::Ignore)
  $w = [double]$bmp.PixelWidth; $h = [double]$bmp.PixelHeight
  $s = 1.0
  if ($MaxPx -gt 0 -and [Math]::Max($w, $h) -gt $MaxPx) { $s = $MaxPx / [Math]::Max($w, $h) }
  $ms = New-Object Windows.Storage.Streams.InMemoryRandomAccessStream
  $enc = Await ([Windows.Graphics.Imaging.BitmapEncoder]::CreateAsync([Windows.Graphics.Imaging.BitmapEncoder]::JpegEncoderId, $ms)) ([Windows.Graphics.Imaging.BitmapEncoder])
  $enc.SetSoftwareBitmap($bmp)
  if ($s -ne 1.0) {
    $enc.BitmapTransform.ScaledWidth = [uint32][Math]::Max(1, [Math]::Round($w * $s))
    $enc.BitmapTransform.ScaledHeight = [uint32][Math]::Max(1, [Math]::Round($h * $s))
    $enc.BitmapTransform.InterpolationMode = [Windows.Graphics.Imaging.BitmapInterpolationMode]::Fant
  }
  AwaitAct ($enc.FlushAsync())
  Say ('P 0 {0} {1}' -f [int]$w, [int]$h)
  Say-Jpeg $ms
}
try {
  Add-Type -AssemblyName System.Runtime.WindowsRuntime
  $null = [Windows.Storage.StorageFile, Windows.Storage, ContentType = WindowsRuntime]
  $null = [Windows.Storage.Streams.InMemoryRandomAccessStream, Windows.Storage.Streams, ContentType = WindowsRuntime]
  $null = [Windows.Graphics.Imaging.BitmapDecoder, Windows.Graphics, ContentType = WindowsRuntime]
  $null = [Windows.Graphics.Imaging.BitmapEncoder, Windows.Graphics, ContentType = WindowsRuntime]
  $null = [Windows.Media.Ocr.OcrEngine, Windows.Foundation, ContentType = WindowsRuntime]
  $null = [Windows.Data.Pdf.PdfDocument, Windows.Data.Pdf, ContentType = WindowsRuntime]
  $null = [Windows.Globalization.Language, Windows.Globalization, ContentType = WindowsRuntime]
  $all = [System.WindowsRuntimeSystemExtensions].GetMethods()
  $script:opT = ($all | Where-Object { $_.Name -eq 'AsTask' -and $_.GetParameters().Count -eq 1 -and $_.GetParameters()[0].ParameterType.Name -eq 'IAsyncOperation` + "`" + `1' })[0]
  $script:actT = ($all | Where-Object { $_.Name -eq 'AsTask' -and $_.GetParameters().Count -eq 1 -and $_.GetParameters()[0].ParameterType.Name -eq 'IAsyncAction' })[0]
  $script:max = [int][Windows.Media.Ocr.OcrEngine]::MaxImageDimension
  if ($Mode -ne 'convert' -and $Mode -ne 'pdfimage') {
    $script:engine = $null
    if ($Lang) { try { $script:engine = [Windows.Media.Ocr.OcrEngine]::TryCreateFromLanguage((New-Object Windows.Globalization.Language($Lang))) } catch { } }
    if ($null -eq $script:engine) { $script:engine = [Windows.Media.Ocr.OcrEngine]::TryCreateFromUserProfileLanguages() }
    if ($null -eq $script:engine) { throw 'Windows OCR has no language installed (Settings > Time & language > Language & region)' }
  }
  switch ($Mode) {
    'image' { Ocr-Decoder (Open-Decoder $Path) 0 }
    'convert' { Convert-Decoder (Open-Decoder $Path) }
    'stdin' {
      $bytes = [Convert]::FromBase64String((New-Object System.IO.StreamReader([Console]::OpenStandardInput())).ReadToEnd().Trim())
      $mem = New-Object System.IO.MemoryStream(, $bytes)
      $st = [System.IO.WindowsRuntimeStreamExtensions]::AsRandomAccessStream($mem)
      Ocr-Decoder (Await ([Windows.Graphics.Imaging.BitmapDecoder]::CreateAsync($st)) ([Windows.Graphics.Imaging.BitmapDecoder])) 0
    }
    'pdfimage' {
      $doc = Open-Pdf $Path
      $pn = [int]$Pages
      if ($pn -lt 1 -or $pn -gt [int]$doc.PageCount) { throw ('the PDF has ' + $doc.PageCount + ' pages') }
      $page = $doc.GetPage([uint32]($pn - 1))
      $sz = $page.Size
      $lim = 800.0
      if ($MaxPx -gt 0) { $lim = [double]$MaxPx }
      $ro = New-Object Windows.Data.Pdf.PdfPageRenderOptions
      if ($sz.Width -ge $sz.Height) { $ro.DestinationWidth = [uint32]$lim } else { $ro.DestinationHeight = [uint32]$lim }
      $ro.BitmapEncoderId = [Windows.Graphics.Imaging.BitmapEncoder]::JpegEncoderId
      $st = New-Object Windows.Storage.Streams.InMemoryRandomAccessStream
      AwaitAct ($page.RenderToStreamAsync($st, $ro))
      Say ('P {0} {1} {2}' -f $pn, [int]$sz.Width, [int]$sz.Height)
      Say-Jpeg $st
    }
    'pdf' {
      $doc = Open-Pdf $Path
      $n = [int]$doc.PageCount
      if ($Pages) { $list = @($Pages.Split(',') | ForEach-Object { [int]$_ }) } else { $list = @(1..$n) }
      foreach ($pn in $list) {
        if ($pn -lt 1 -or $pn -gt $n) { continue }
        $page = $doc.GetPage([uint32]($pn - 1))
        $want = 2000.0
        if ($Width -gt 0) { $want = [double]$Width }
        $sz = $page.Size
        if ($sz.Width -gt 0 -and $sz.Height * $want / $sz.Width -gt $script:max) { $want = $script:max * $sz.Width / $sz.Height }
        if ($want -gt $script:max) { $want = $script:max }
        $ro = New-Object Windows.Data.Pdf.PdfPageRenderOptions
        $ro.DestinationWidth = [uint32]$want
        $st = New-Object Windows.Storage.Streams.InMemoryRandomAccessStream
        AwaitAct ($page.RenderToStreamAsync($st, $ro))
        $page.Dispose()
        Ocr-Decoder (Await ([Windows.Graphics.Imaging.BitmapDecoder]::CreateAsync($st)) ([Windows.Graphics.Imaging.BitmapDecoder])) $pn
      }
    }
  }
} catch {
  Say ('E ' + ($_.Exception.Message -replace '\s+', ' '))
  exit 1
}
`

type ocrWord struct {
	X, Y, W, H int
	Text       string
}

type ocrLine struct {
	Text  string
	Words []ocrWord
}

type ocrPage struct {
	N, W, H int
	Lines   []ocrLine
}

func (p ocrPage) text() string {
	var b strings.Builder
	for i, l := range p.Lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(l.Text)
	}
	return b.String()
}

type ocrReq struct {
	mode           string // image, stdin, pdf, convert
	path           string
	pages          []int
	width, maxPx   int
	boxes, upscale bool
	stdin          []byte
}

var ocrScriptMu sync.Mutex

func ocrScriptPath() (string, error) {
	ocrScriptMu.Lock()
	defer ocrScriptMu.Unlock()
	p := filepath.Join(appHome, "scripts", "winocr.ps1")
	if b, err := os.ReadFile(p); err == nil && string(b) == winOCRScript {
		return p, nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	return p, os.WriteFile(p, []byte(winOCRScript), 0o644)
}

func psExe() string {
	if r := os.Getenv("SystemRoot"); r != "" {
		if p := filepath.Join(r, "System32", "WindowsPowerShell", "v1.0", "powershell.exe"); exists(p) {
			return p
		}
	}
	return "powershell.exe"
}

type tailBuf struct{ b []byte }

func (t *tailBuf) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > 4096 {
		t.b = t.b[len(t.b)-4096:]
	}
	return len(p), nil
}

// runWinOCR runs the script; each gets every finished page or image. It returns
// the JPEG from convert mode.
func runWinOCR(ctx context.Context, r ocrReq, each func(ocrPage)) ([]byte, error) {
	if !isWin {
		return nil, errors.New("OCR needs Windows")
	}
	script, err := ocrScriptPath()
	if err != nil {
		return nil, err
	}
	args := []string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", script, "-Mode", r.mode}
	if r.path != "" {
		args = append(args, "-Path", r.path)
	}
	if len(r.pages) > 0 {
		ps := make([]string, len(r.pages))
		for i, n := range r.pages {
			ps[i] = strconv.Itoa(n)
		}
		args = append(args, "-Pages", strings.Join(ps, ","))
	}
	if r.width > 0 {
		args = append(args, "-Width", strconv.Itoa(r.width))
	}
	if r.maxPx > 0 {
		args = append(args, "-MaxPx", strconv.Itoa(r.maxPx))
	}
	if r.boxes {
		args = append(args, "-Boxes", "1")
	}
	if r.upscale {
		args = append(args, "-Upscale", "1")
	}
	cmd := quietCmd(psExe(), args...)
	if r.stdin != nil {
		cmd.Stdin = strings.NewReader(base64.StdEncoding.EncodeToString(r.stdin))
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr := &tailBuf{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("couldn't start PowerShell for OCR: %v", err)
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			cmd.Process.Kill()
		case <-done:
		}
	}()
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1<<20), 256<<20)
	var cur *ocrPage
	var img []byte
	var msg string
	flush := func() {
		if cur != nil && each != nil {
			each(*cur)
		}
		cur = nil
	}
	for sc.Scan() {
		line := sc.Text()
		if len(line) < 2 {
			continue
		}
		rest := line[2:]
		switch line[0] {
		case 'P':
			flush()
			f := strings.Fields(rest)
			cur = &ocrPage{}
			if len(f) >= 3 {
				cur.N, _ = strconv.Atoi(f[0])
				cur.W, _ = strconv.Atoi(f[1])
				cur.H, _ = strconv.Atoi(f[2])
			}
		case 'L':
			if cur != nil {
				cur.Lines = append(cur.Lines, ocrLine{Text: strings.TrimSpace(rest)})
			}
		case 'W':
			if cur != nil && len(cur.Lines) > 0 {
				if f := strings.SplitN(rest, " ", 5); len(f) == 5 {
					var w ocrWord
					w.X, _ = strconv.Atoi(f[0])
					w.Y, _ = strconv.Atoi(f[1])
					w.W, _ = strconv.Atoi(f[2])
					w.H, _ = strconv.Atoi(f[3])
					w.Text = f[4]
					l := &cur.Lines[len(cur.Lines)-1]
					l.Words = append(l.Words, w)
				}
			}
		case 'J':
			img, _ = base64.StdEncoding.DecodeString(strings.TrimSpace(rest))
		case 'E':
			msg = strings.TrimSpace(rest)
		}
	}
	flush()
	werr := cmd.Wait()
	close(done)
	switch {
	case msg != "":
		return img, errors.New("Windows OCR: " + msg)
	case ctx.Err() != nil:
		return img, errors.New("OCR took too long")
	case werr != nil:
		return img, fmt.Errorf("Windows OCR failed: %v %s", werr, clip(strings.TrimSpace(string(stderr.b)), 300))
	}
	return img, nil
}

// ---------- cache ----------

var (
	ocrMu      sync.Mutex
	ocrRunning = map[string]chan struct{}{}
	ocrErrs    = map[string]error{}
)

func ocrCachePath(p string, fi fs.FileInfo) string {
	sum := sha1.Sum([]byte(normCase(p) + "|" + strconv.FormatInt(fi.Size(), 10) + "|" + strconv.FormatInt(fi.ModTime().UnixNano(), 10)))
	return filepath.Join(appHome, "cache", "ocr", hex.EncodeToString(sum[:10])+".json")
}

func loadOCRCache(cp string) map[int]string {
	out := map[int]string{}
	if b, err := os.ReadFile(cp); err == nil {
		var c struct {
			Pages map[string]string `json:"pages"`
		}
		if json.Unmarshal(b, &c) == nil {
			for k, v := range c.Pages {
				if n, err := strconv.Atoi(k); err == nil {
					out[n] = v
				}
			}
		}
	}
	return out
}

func saveOCRCache(cp string, m map[int]string) {
	pages := map[string]string{}
	for k, v := range m {
		pages[strconv.Itoa(k)] = v
	}
	os.MkdirAll(filepath.Dir(cp), 0o755)
	writeJSONAtomic(cp, map[string]any{"pages": pages})
}

// ocrFile returns OCR text for pages of a PDF (or page 0 of an image), from the cache
// or by running Windows OCR, waiting at most wait. Pages not done by then keep going
// in the background and come back in pending. wait <= 0 reads the cache only.
func ocrFile(p string, fi fs.FileInfo, pages []int, isPDF bool, wait time.Duration) (map[int]string, []int, error) {
	cp := ocrCachePath(p, fi)
	missingOf := func(have map[int]string) []int {
		var miss []int
		for _, n := range pages {
			if _, ok := have[n]; !ok {
				miss = append(miss, n)
			}
		}
		return miss
	}
	ocrMu.Lock()
	have := loadOCRCache(cp)
	missing := missingOf(have)
	if len(missing) == 0 || wait <= 0 || !isWin {
		ocrMu.Unlock()
		return have, missing, nil
	}
	done, running := ocrRunning[cp]
	if !running {
		done = make(chan struct{})
		ocrRunning[cp] = done
		delete(ocrErrs, cp)
		todo := append([]int(nil), missing...)
		go func() {
			req := ocrReq{mode: "image", path: p, upscale: true}
			if isPDF {
				req = ocrReq{mode: "pdf", path: p, pages: todo}
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(60+20*len(todo))*time.Second)
			_, err := runWinOCR(ctx, req, func(pg ocrPage) {
				ocrMu.Lock()
				m := loadOCRCache(cp)
				m[pg.N] = pg.text()
				saveOCRCache(cp, m)
				ocrMu.Unlock()
			})
			cancel()
			ocrMu.Lock()
			if err != nil {
				ocrErrs[cp] = err
			}
			delete(ocrRunning, cp)
			ocrMu.Unlock()
			close(done)
		}()
	}
	ocrMu.Unlock()
	select {
	case <-done:
	case <-time.After(wait):
	}
	ocrMu.Lock()
	defer ocrMu.Unlock()
	have = loadOCRCache(cp)
	missing = missingOf(have)
	if _, still := ocrRunning[cp]; !still && len(missing) > 0 {
		if err := ocrErrs[cp]; err != nil {
			return have, missing, err
		}
	}
	return have, missing, nil
}
