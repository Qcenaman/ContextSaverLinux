package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestOfficeRoundTrip(t *testing.T) {
	dir := os.Getenv("TS_OFFICE_OUT")
	if dir == "" {
		t.Skip("set TS_OFFICE_OUT to a folder with pic.png and pic.jpg")
	}
	appHome = filepath.Join(dir, "home")
	call := func(args map[string]any) string {
		t.Helper()
		b, _ := json.Marshal(args)
		out, err := toolOffice(b)
		if err != nil {
			t.Fatalf("%v: %v", args["action"], err)
		}
		t.Log(out)
		return out
	}
	md := "# Quarterly Report\n\nIntro with **bold**, *italic*, `code` and a [link](https://example.com). Señor café – “quotes” €5.\n\n## Items\n\n- first\n  - nested\n- second\n\n1. one\n2. two\n\n| Name | Qty | Note |\n|---|---|---|\n| Apple | 3 | red |\n| Pear | 10 | a much longer note that should wrap inside its cell nicely |\n\n![](pic.png)\n\n```\nfunc main() {}\n```\n\n---\n\n# Second page\n\nMore text."
	for _, ext := range []string{"docx", "pdf", "pptx"} {
		os.Remove(filepath.Join(dir, "t."+ext))
		call(map[string]any{"action": "create", "path": filepath.Join(dir, "t."+ext), "content": md})
	}
	os.Remove(filepath.Join(dir, "t.xlsx"))
	call(map[string]any{"action": "create", "path": filepath.Join(dir, "t.xlsx"), "sheets": []any{
		map[string]any{"name": "Data", "rows": []any{[]any{"Item", "Qty", "Price", "Date"}, []any{"Apple", 3, 1.25, "2026-10-27"}, []any{"Pear", "10", 0.5, "2026-01-02"}, []any{"Total", "=SUM(B2:B3)", nil, nil}}},
		map[string]any{"name": "Notes", "rows": []any{[]any{"hello"}}}}})
	call(map[string]any{"action": "edit", "path": filepath.Join(dir, "t.xlsx"), "cells": map[string]any{"C4": "=SUMPRODUCT(B2:B3,C2:C3)", "Notes!B2": "x"}, "append_rows": []any{[]any{"Plum", 7, 2}}, "replace": []any{map[string]any{"find": "Pear", "with": "Peach"}}})
	call(map[string]any{"action": "edit", "path": filepath.Join(dir, "t.docx"), "replace": []any{map[string]any{"find": "Quarterly", "with": "Annual"}}, "delete_paragraphs": []string{"More text"}, "content": "## Added\n\n1. again one\n2. again two\n\n- bullet"})
	call(map[string]any{"action": "edit", "path": filepath.Join(dir, "t.docx"), "insert_after": "Intro with", "content": "Inserted paragraph."})
	call(map[string]any{"action": "edit", "path": filepath.Join(dir, "t.pptx"), "replace": []any{map[string]any{"find": "Second page", "with": "Slide Two"}}, "content": "# New slide\n\n- a\n- b", "delete_slides": "1"})
	call(map[string]any{"action": "edit", "path": filepath.Join(dir, "t.pdf"), "to": filepath.Join(dir, "t2.pdf"), "overwrite": true, "pages": "2,1", "stamp": "Page {n} of {total}", "from": []string{filepath.Join(dir, "t.pdf") + ":1", filepath.Join(dir, "pic.jpg")}})
	call(map[string]any{"action": "edit", "path": filepath.Join(dir, "t2.pdf"), "to": filepath.Join(dir, "t3.pdf"), "overwrite": true, "rotate": 90, "apply_to": "1", "stamp": "DRAFT", "stamp_at": "center"})
	call(map[string]any{"action": "edit", "path": filepath.Join(dir, "t3.pdf"), "split": true, "to": filepath.Join(dir, "split"), "overwrite": true})
	for _, f := range []string{"t.docx", "t.xlsx", "t.pptx", "t2.pdf", "t3.pdf"} {
		t.Log("READ " + f + ":\n" + call(map[string]any{"action": "read", "path": filepath.Join(dir, f), "max_chars": 3000}))
	}
}
