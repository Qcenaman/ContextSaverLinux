package main

import (
	"fmt"
	"strings"
	"testing"
)

func testPDF(content string, badXref bool) []byte {
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 300 200] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content),
	}
	var b strings.Builder
	b.WriteString("%PDF-1.4\n")
	offs := make([]int, len(objs))
	for i, o := range objs {
		offs[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	x := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, o := range offs {
		fmt.Fprintf(&b, "%010d 00000 n \n", o)
	}
	if badXref {
		x = 7
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, x)
	return []byte(b.String())
}

func TestPDFText(t *testing.T) {
	content := "BT /F1 12 Tf 20 150 Td (Hello) Tj 30 0 Td (World) Tj 0 -20 Td [(Line) -250 (two)] TJ ET"
	for _, bad := range []bool{false, true} {
		d, err := parsePDF(testPDF(content, bad))
		if err != nil {
			t.Fatal(err)
		}
		tx := readPDFText(d, nil, 0)
		if len(tx.pages) != 1 || tx.pages[0] != "Hello World\nLine two" {
			t.Fatalf("bad xref %v: got %q", bad, tx.pages)
		}
	}
}

func TestParsePages(t *testing.T) {
	got, err := parsePages("1, 4,7-9")
	if err != nil || len(got) != 5 || !got[1] || !got[8] || got[5] {
		t.Fatalf("got %v %v", got, err)
	}
	if _, err := parsePages("5-2"); err == nil {
		t.Fatal("5-2 should fail")
	}
}
