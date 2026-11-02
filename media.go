package main

import (
	"archive/zip"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// media: audio and video as text. info (ffprobe), transcript (local Whisper on the GPU,
// cached), frames (stills saved to a folder, with their OCR text).

const whisperModelURL = "https://huggingface.co/ggerganov/whisper.cpp/resolve/main/ggml-large-v3-turbo.bin"

var (
	mediaMu      sync.Mutex
	mediaRunning = map[string]chan struct{}{}
	mediaErrs    = map[string]error{}
)

func whisperDir() string { return filepath.Join(appHome, "whisper") }

func mediaKey(p string) (string, error) {
	fi, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	sum := sha1.Sum([]byte(normCase(p) + "|" + strconv.FormatInt(fi.Size(), 10) + "|" + strconv.FormatInt(fi.ModTime().UnixNano(), 10)))
	return hex.EncodeToString(sum[:10]), nil
}

func mediaCache() string { return filepath.Join(appHome, "cache", "media") }

func runMediaCmd(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := quietCmd(name, args...)
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			if cmd.Process != nil {
				cmd.Process.Kill()
			}
		case <-done:
		}
	}()
	out, err := cmd.CombinedOutput()
	close(done)
	return out, err
}

func mediaInfo(p string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := runMediaCmd(ctx, "ffprobe", "-v", "error", "-show_entries", "format=duration:stream=codec_type,codec_name,width,height,channels,sample_rate", "-of", "json", p)
	if err != nil {
		return "", fmt.Errorf("ffprobe: %v %s", err, clip(string(out), 200))
	}
	var r struct {
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
		Streams []struct {
			Type     string `json:"codec_type"`
			Codec    string `json:"codec_name"`
			W        int    `json:"width"`
			H        int    `json:"height"`
			Channels int    `json:"channels"`
			Rate     string `json:"sample_rate"`
		} `json:"streams"`
	}
	json.Unmarshal(out, &r)
	d, _ := strconv.ParseFloat(r.Format.Duration, 64)
	s := fmt.Sprintf("%s: %s long", filepath.Base(p), hd(time.Duration(d*float64(time.Second))))
	for _, st := range r.Streams {
		switch st.Type {
		case "video":
			s += fmt.Sprintf(", video %s %dx%d", st.Codec, st.W, st.H)
		case "audio":
			s += fmt.Sprintf(", audio %s %d ch %s Hz", st.Codec, st.Channels, st.Rate)
		}
	}
	return s, nil
}

func download(ctx context.Context, url, dst string) error {
	if exists(dst) {
		return nil
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s answered %s", url, resp.Status)
	}
	tmp := dst + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, resp.Body)
	f.Close()
	if err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// ensureWhisper downloads whisper.cpp (CUDA build) and the model on first use.
func ensureWhisper(ctx context.Context) (string, string, error) {
	dir := whisperDir()
	os.MkdirAll(dir, 0o755)
	model := filepath.Join(dir, "ggml-large-v3-turbo.bin")
	var cli string
	filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && strings.EqualFold(fi.Name(), "whisper-cli.exe") {
			cli = p
		}
		return nil
	})
	if cli == "" {
		var rs []struct {
			Assets []struct {
				Name string `json:"name"`
				URL  string `json:"browser_download_url"`
			} `json:"assets"`
		}
		req, _ := http.NewRequestWithContext(ctx, "GET", "https://api.github.com/repos/ggml-org/whisper.cpp/releases?per_page=10", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return "", "", err
		}
		json.NewDecoder(resp.Body).Decode(&rs)
		resp.Body.Close()
		zipURL := ""
		for _, r := range rs {
			for _, a := range r.Assets {
				if strings.HasPrefix(a.Name, "whisper-cublas-12") && strings.HasSuffix(a.Name, "-bin-x64.zip") && zipURL == "" {
					zipURL = a.URL
				}
			}
		}
		if zipURL == "" {
			return "", "", errors.New("couldn't find a whisper.cpp CUDA build to download")
		}
		zp := filepath.Join(dir, "whisper.zip")
		if err := download(ctx, zipURL, zp); err != nil {
			return "", "", err
		}
		zr, err := zip.OpenReader(zp)
		if err != nil {
			return "", "", err
		}
		for _, f := range zr.File {
			out := filepath.Join(dir, "bin", filepath.FromSlash(f.Name))
			if !strings.HasPrefix(out, filepath.Join(dir, "bin")) || f.FileInfo().IsDir() {
				continue
			}
			os.MkdirAll(filepath.Dir(out), 0o755)
			rc, err := f.Open()
			if err != nil {
				continue
			}
			w, err := os.Create(out)
			if err == nil {
				io.Copy(w, rc)
				w.Close()
			}
			rc.Close()
			if strings.EqualFold(filepath.Base(out), "whisper-cli.exe") {
				cli = out
			}
		}
		zr.Close()
		if cli == "" {
			return "", "", errors.New("whisper-cli.exe wasn't in the download")
		}
	}
	if err := download(ctx, whisperModelURL, model); err != nil {
		return "", "", err
	}
	return cli, model, nil
}

func transcribe(p, key, lang string, stamps bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Hour)
	defer cancel()
	cli, model, err := ensureWhisper(ctx)
	if err != nil {
		return err
	}
	dir := mediaCache()
	os.MkdirAll(dir, 0o755)
	wav := filepath.Join(dir, key+".wav")
	if out, err := runMediaCmd(ctx, "ffmpeg", "-y", "-v", "error", "-i", p, "-vn", "-ac", "1", "-ar", "16000", "-c:a", "pcm_s16le", wav); err != nil {
		return fmt.Errorf("ffmpeg: %v %s", err, clip(string(out), 200))
	}
	defer os.Remove(wav)
	if lang == "" {
		lang = "auto"
	}
	base := filepath.Join(dir, key)
	out, err := runMediaCmd(ctx, cli, "-m", model, "-f", wav, "-l", lang, "-osrt", "-otxt", "-of", base, "-np")
	if err != nil {
		return fmt.Errorf("whisper: %v %s", err, clip(string(out), 300))
	}
	return nil
}

func mediaTranscript(p, lang string, stamps, force bool, wait time.Duration, maxc int) (string, error) {
	key, err := mediaKey(p)
	if err != nil {
		return "", err
	}
	txt := filepath.Join(mediaCache(), key+".txt")
	if stamps {
		txt = filepath.Join(mediaCache(), key+".srt")
	}
	if b, err := os.ReadFile(txt); err == nil {
		body, note := capText(strings.TrimSpace(string(b)), maxc, filepath.Base(p)+" transcript")
		return fmt.Sprintf("Transcript of %s (full text: %s)\n%s%s", filepath.Base(p), txt, body, note), nil
	}
	if !force {
		if w := gpuWarning(); w != "" {
			return "", errors.New(strings.Replace(w, "call local_ai again", "call media again", 1))
		}
	}
	mediaMu.Lock()
	done, running := mediaRunning[key]
	if !running {
		done = make(chan struct{})
		mediaRunning[key] = done
		delete(mediaErrs, key)
		go func() {
			err := transcribe(p, key, lang, stamps)
			mediaMu.Lock()
			if err != nil {
				mediaErrs[key] = err
			}
			delete(mediaRunning, key)
			mediaMu.Unlock()
			close(done)
		}()
	}
	mediaMu.Unlock()
	select {
	case <-done:
	case <-time.After(wait):
		first := ""
		if !exists(filepath.Join(whisperDir(), "ggml-large-v3-turbo.bin")) {
			first = " (first use: downloading Whisper and its model, about 2.2 GB)"
		}
		return fmt.Sprintf("Transcribing %s in the background%s; call media transcript again for the result.", filepath.Base(p), first), nil
	}
	mediaMu.Lock()
	err = mediaErrs[key]
	mediaMu.Unlock()
	if err != nil {
		return "", err
	}
	return mediaTranscript(p, lang, stamps, true, 0, maxc)
}

// mediaFrames saves stills from a video and returns their OCR text.
func mediaFrames(p string, n int, every float64, maxc int) (string, error) {
	key, err := mediaKey(p)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(mediaCache(), key+"-frames")
	os.MkdirAll(dir, 0o755)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if every <= 0 {
		info, _ := runMediaCmd(ctx, "ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "default=nw=1:nk=1", p)
		d, _ := strconv.ParseFloat(strings.TrimSpace(string(info)), 64)
		if n <= 0 {
			n = 8
		}
		if d <= 0 {
			return "", errors.New("couldn't read the video's length")
		}
		every = d / float64(n)
	}
	if out, err := runMediaCmd(ctx, "ffmpeg", "-y", "-v", "error", "-i", p, "-vf", fmt.Sprintf("fps=1/%g,scale='min(1280,iw)':-2", every), "-frames:v", "60", filepath.Join(dir, "frame-%03d.jpg")); err != nil {
		return "", fmt.Errorf("ffmpeg: %v %s", err, clip(string(out), 200))
	}
	files, _ := filepath.Glob(filepath.Join(dir, "frame-*.jpg"))
	sort.Strings(files)
	var b strings.Builder
	fmt.Fprintf(&b, "%d frames from %s in %s (one every %.1fs). Use read mode image on one to look, or local_ai on the folder to describe them.\n", len(files), filepath.Base(p), dir, every)
	for i, f := range files {
		t, err := extractTextOpt(f, 20000, textOpts{ocrWait: 20 * time.Second})
		if err != nil || strings.HasPrefix(t, "[no text") {
			continue
		}
		fmt.Fprintf(&b, "[%s at %.0fs] %s\n", filepath.Base(f), float64(i)*every+every/2, strings.ReplaceAll(clip(t, 300), "\n", " / "))
	}
	out, note := capText(strings.TrimRight(b.String(), "\n"), maxc, filepath.Base(p)+" frames")
	return out + note, nil
}

func toolMedia(raw json.RawMessage) (string, error) {
	var a struct {
		Action     string  `json:"action"`
		Path       string  `json:"path"`
		Language   string  `json:"language"`
		Timestamps bool    `json:"timestamps"`
		Frames     int     `json:"frames"`
		Every      float64 `json:"every"`
		Force      bool    `json:"force"`
		MaxChars   int     `json:"max_chars"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", err
	}
	p, err := expandPath(a.Path)
	if err != nil {
		return "", err
	}
	if err := checkReadable(p); err != nil {
		return "", err
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return "", errors.New("ffmpeg isn't installed (winget install ffmpeg)")
	}
	maxc := a.MaxChars
	if maxc <= 0 {
		maxc = 6000
	}
	maxc = min(maxc, 30000)
	switch strings.ToLower(a.Action) {
	case "info", "":
		return mediaInfo(p)
	case "transcript":
		return mediaTranscript(p, a.Language, a.Timestamps, a.Force, 45*time.Second, maxc)
	case "frames":
		return mediaFrames(p, a.Frames, a.Every, maxc)
	}
	return "", errors.New("action must be info, transcript or frames")
}
