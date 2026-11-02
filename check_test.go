package main

import (
	"encoding/xml"
	"testing"
)

func TestCkIgnored(t *testing.T) {
	ign := []string{"data/", "tokensaver.exe"}
	cases := map[string]bool{"data/x/y.js": true, "data/": true, "tokensaver.exe": true, "dat/x.js": false, "check.go": false}
	for p, want := range cases {
		if got := ckIgnored(p, ign); got != want {
			t.Errorf("ckIgnored(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestCkGoLine(t *testing.T) {
	m := ckGoLine.FindStringSubmatch(`.\check.go:12:3: this value of err is never used (SA4006)`)
	if m == nil || m[1] != `.\check.go` || m[2] != "12" || m[5] != "SA4006" {
		t.Fatalf("staticcheck line: %q", m)
	}
	m = ckGoLine.FindStringSubmatch(`vet: mcp.go:5:2: unreachable code`)
	if m == nil || m[1] != "mcp.go" || m[5] != "" {
		t.Fatalf("vet line: %q", m)
	}
}

func TestCkLintXML(t *testing.T) {
	var r ckLintXML
	x := `<issues format="6"><issue id="SdCardPath" severity="Warning" message="Do not hardcode"><location file="C:\p\app\Fs.kt" line="10" column="5"/></issue></issues>`
	if err := xml.Unmarshal([]byte(x), &r); err != nil || len(r.Issues) != 1 || r.Issues[0].ID != "SdCardPath" || r.Issues[0].Locations[0].Line != 10 {
		t.Fatalf("lint xml: %v %+v", err, r)
	}
}
