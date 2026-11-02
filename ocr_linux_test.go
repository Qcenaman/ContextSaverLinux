package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func need(t *testing.T, progs ...string) {
	t.Helper()
	for _, p := range progs {
		if _, err := exec.LookPath(p); err != nil {
			t.Skip(p + " not installed")
		}
	}
}

func TestLinuxOCRReadsPicture(t *testing.T) {
	need(t, "tesseract", "convert")
	p := filepath.Join(t.TempDir(), "sign.png")
	if out, err := exec.Command("convert", "-size", "700x140", "xc:white", "-fill", "black", "-pointsize", "56",
		"-annotate", "+20+90", "Hello Linux OCR", p).CombinedOutput(); err != nil {
		t.Skipf("convert: %v %s", err, out)
	}
	var pg ocrPage
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := runWinOCR(ctx, ocrReq{mode: "image", path: p, upscale: true}, func(x ocrPage) { pg = x }); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pg.text(), "Hello Linux OCR") {
		t.Fatalf("OCR read %q", pg.text())
	}
	if len(pg.Lines) == 0 || len(pg.Lines[0].Words) < 3 || pg.Lines[0].Words[0].W <= 0 || pg.W != 700 {
		t.Fatalf("no word boxes: %+v", pg)
	}
}

func TestPDFPageAndConvert(t *testing.T) {
	need(t, "pdftoppm")
	codeHome(t)
	dir := t.TempDir()
	pdf := filepath.Join(dir, "doc.pdf")
	raw, _ := json.Marshal(map[string]any{"action": "create", "path": pdf, "content": "# Title\n\nSome text on page one."})
	if _, err := toolOffice(raw); err != nil {
		t.Fatal(err)
	}
	pv, err := pdfPagePreview(pdf, 1, 400)
	if err != nil || len(pv.jpg) < 100 || max(pv.w, pv.h) > 400 {
		t.Fatalf("page image: %v", err)
	}
	if sofficePath() == "" {
		t.Skip("LibreOffice not installed")
	}
	docx := filepath.Join(dir, "letter.docx")
	raw, _ = json.Marshal(map[string]any{"action": "create", "path": docx, "content": "# Hi\n\nConverted by LibreOffice."})
	if _, err := toolOffice(raw); err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(map[string]any{"action": "convert", "path": docx, "to": ".pdf"})
	out, err := toolOffice(raw)
	if err != nil || !exists(filepath.Join(dir, "letter.pdf")) || !strings.Contains(out, "converted") {
		t.Fatalf("%v %s", err, out)
	}
}

func TestAtspiScriptCompiles(t *testing.T) {
	need(t, "python3")
	codeHome(t)
	p, err := atspiScriptPath()
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("python3", "-m", "py_compile", p).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	os.Remove(p)
}

func TestItemsTextAndKeys(t *testing.T) {
	out := "10\t10\t20\tText\tName:\t\n200\t12\t20\tEdit\tName\tAna\n10\t60\t20\tButton\tSave\t\n"
	s, err := uiaItemsText(out)
	if err != nil || s != "Name:  Name: Ana\n[Save]" {
		t.Fatalf("%v %q", err, s)
	}
	if _, err := uiaItemsText("E nothing there\n"); err == nil {
		t.Fatal("error line ignored")
	}
	for name, want := range map[string]string{"enter": "Return", "ctrl": "ctrl", "a": "a", "f5": "F5", "pagedown": "Next", "num3": "KP_3"} {
		vk, _ := keyVK(name)
		if got := keysym(vk); got != want {
			t.Errorf("%s -> %q, want %q", name, got, want)
		}
	}
}
