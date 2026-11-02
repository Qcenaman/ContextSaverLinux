package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// LibreOffice stands in for Word, Excel and PowerPoint on Linux (headless, with its own
// profile so an open LibreOffice window doesn't block the conversion).

func sofficePath() string {
	for _, n := range []string{"soffice", "libreoffice"} {
		if p, err := exec.LookPath(n); err == nil {
			return p
		}
	}
	for _, p := range []string{"/opt/libreoffice/program/soffice", "/snap/bin/libreoffice"} {
		if exists(p) {
			return p
		}
	}
	return ""
}

// sofficeFilters picks LibreOffice's export filter where the extension alone is ambiguous.
var sofficeFilters = map[string]string{".txt": "txt:Text (encoded):UTF8", ".csv": "csv:Text - txt - csv (StarCalc):44,34,76"}

func sofficeConvert(src, dst string) error {
	so := sofficePath()
	if so == "" {
		return errors.New("convert needs LibreOffice (" + installHint("libreoffice-writer libreoffice-calc libreoffice-impress") + ")")
	}
	de := strings.ToLower(filepath.Ext(dst))
	if de == "" {
		return errors.New("to needs an extension, e.g. .pdf")
	}
	tmp, err := os.MkdirTemp("", "contextsaver-soffice")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	format := strings.TrimPrefix(de, ".")
	if f, ok := sofficeFilters[de]; ok {
		format = f
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	c := exec.CommandContext(ctx, so, "-env:UserInstallation=file://"+filepath.Join(tmp, "profile"), "--headless", "--norestore",
		"--convert-to", format, "--outdir", tmp, src)
	out, err := c.CombinedOutput()
	made := filepath.Join(tmp, strings.TrimSuffix(filepath.Base(src), filepath.Ext(src))+de)
	if !exists(made) {
		if err == nil {
			err = errors.New(clip(strings.TrimSpace(string(out)), 300))
		}
		return fmt.Errorf("LibreOffice couldn't convert to %s: %v", de, err)
	}
	os.MkdirAll(filepath.Dir(dst), 0o755)
	if err := os.Rename(made, dst); err != nil {
		return copyPlain(made, dst)
	}
	return nil
}
