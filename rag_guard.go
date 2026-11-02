package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Guards for the RAG index. The index under ragDir() is shared with
// ContextSaver, so one bad config write (a test pointing the roots at a temp
// folder, say) is enough for the next update to drop every indexed file.

const ragConfigBackupsKept = 10

// saveRagConfig writes config.json. The previous version is kept as
// config.json.bak plus a dated copy in config-backups (the last few), and
// roots inside the temp folder are refused for the real index.
func saveRagConfig(c ragConfig) error { return saveRagConfigOpt(c, false) }

func saveRagConfigOpt(c ragConfig, confirm bool) error {
	if !confirm {
		if err := ragRootsTempCheck(c.Roots, ragDir()); err != nil {
			return err
		}
	}
	os.MkdirAll(ragDir(), 0o755)
	p := filepath.Join(ragDir(), "config.json")
	if old, err := os.ReadFile(p); err == nil && len(bytes.TrimSpace(old)) > 0 && ragConfigChanged(old, c) {
		if err := os.WriteFile(p+".bak", old, 0o644); err != nil {
			return fmt.Errorf("rag config not saved: could not back up the old one: %v", err)
		}
		bd := filepath.Join(ragDir(), "config-backups")
		if os.MkdirAll(bd, 0o755) == nil {
			os.WriteFile(filepath.Join(bd, "config-"+time.Now().Format("20060102-150405.000000000")+".json"), old, 0o644)
			pruneRagConfigBackups(bd)
		}
	}
	return writeJSONAtomic(p, c)
}

// ragConfigChanged: the new config differs from the file (the timestamp aside).
func ragConfigChanged(old []byte, c ragConfig) bool {
	var o ragConfig
	if json.Unmarshal(old, &o) != nil {
		return true
	}
	o.Updated, c.Updated = "", ""
	a, _ := json.Marshal(o)
	b, _ := json.Marshal(c)
	return !bytes.Equal(a, b)
}

func pruneRagConfigBackups(dir string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var names []string
	for _, e := range ents {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "config-") && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for len(names) > ragConfigBackupsKept {
		os.Remove(filepath.Join(dir, names[0]))
		names = names[1:]
	}
}

func ragRootsIn(file string) []string {
	b, err := os.ReadFile(filepath.Join(ragDir(), file))
	if err != nil {
		return nil
	}
	var c ragConfig
	if json.Unmarshal(b, &c) != nil {
		return nil
	}
	return c.Roots
}

// ragSavedRoots: the roots written in config.json (no defaults filled in).
func ragSavedRoots() []string { return ragRootsIn("config.json") }

// ragBackupRoots: the roots before the last config change (config.json.bak).
func ragBackupRoots() []string { return ragRootsIn("config.json.bak") }

// ---------- suspicious roots ----------

func ragTempDirs() []string {
	var ds []string
	for _, d := range []string{os.TempDir(), os.Getenv("TEMP"), os.Getenv("TMP")} {
		if d == "" {
			continue
		}
		ds = append(ds, filepath.Clean(d))
		if l, err := filepath.EvalSymlinks(d); err == nil {
			ds = append(ds, l)
		}
	}
	if la := os.Getenv("LOCALAPPDATA"); la != "" {
		ds = append(ds, filepath.Join(la, "Temp"))
	}
	if !isWin {
		ds = append(ds, "/tmp", "/var/tmp")
	}
	return ds
}

// underTemp: p is the temp folder or inside it.
func underTemp(p string) bool {
	low := strings.ToLower(strings.ReplaceAll(filepath.Clean(p), `\`, "/")) + "/"
	if strings.Contains(low, "/appdata/local/temp/") {
		return true
	}
	for _, d := range ragTempDirs() {
		if hasPathPrefix(p, d) {
			return true
		}
	}
	return false
}

// ragRootsTempCheck refuses roots inside the temp folder for a real index
// (one outside the temp folder): that mix is what a test leaking into the
// live config looks like. A test's own index in the temp folder may use them.
func ragRootsTempCheck(roots []string, indexDir string) error {
	if underTemp(indexDir) {
		return nil
	}
	var bad []string
	for _, r := range roots {
		if underTemp(r) {
			bad = append(bad, r)
		}
	}
	if len(bad) == 0 {
		return nil
	}
	return fmt.Errorf("refused: rag folders inside the temp folder (%s) for the real index at %s. Nothing was changed. Pass confirm:true only if the user really wants it", strings.Join(bad, ", "), indexDir)
}

// ragRootsReplaceCheck refuses a config call that swaps out every saved root
// at once, which would make the next update drop the whole index.
func ragRootsReplaceCheck(old, nw []string) error {
	if len(old) == 0 {
		return nil
	}
	for _, o := range old {
		for _, n := range nw {
			if samePath(o, n) {
				return nil
			}
		}
	}
	return fmt.Errorf("refused: this replaces every indexed folder at once (now: %s; new: %s), so the next update would drop the whole index. Nothing was changed. Pass confirm:true if the user really wants it", strings.Join(old, ", "), strings.Join(nw, ", "))
}

// ---------- mass removal ----------

// ragMassRemoval: an update would drop more than half of the indexed files.
func ragMassRemoval(removed, total int) bool {
	return total > 0 && removed > 0 && removed*2 > total
}

func ragRootsDiff(prev, now []string) (gone, added []string) {
	has := func(list []string, p string) bool {
		for _, x := range list {
			if samePath(x, p) {
				return true
			}
		}
		return false
	}
	for _, p := range prev {
		if !has(now, p) {
			gone = append(gone, p)
		}
	}
	for _, p := range now {
		if !has(prev, p) {
			added = append(added, p)
		}
	}
	return
}

func ragMassRemovalText(removed, total int, roots []string, dry bool) string {
	var b strings.Builder
	if dry {
		b.WriteString("WARNING: ")
	} else {
		b.WriteString("REFUSED: NOT started. ")
	}
	fmt.Fprintf(&b, "This update would remove %s of the %s indexed files (%d%%). Folders now: %s.", hc(int64(removed)), hc(int64(total)), removed*100/total, strings.Join(roots, ", "))
	if prev := ragBackupRoots(); prev != nil {
		if gone, added := ragRootsDiff(prev, roots); len(gone)+len(added) > 0 {
			fmt.Fprintf(&b, " Changed since the previous config (config.json.bak): removed %s; added %s.", orNone(gone), orNone(added))
		}
	}
	if underTempAny(roots) {
		b.WriteString(" Some folders are inside the temp folder.")
	}
	if dry {
		b.WriteString(" A real update refuses this unless confirm:true.")
	} else {
		b.WriteString(" Check the folders with the user (rag config can restore them). Run update with confirm:true only if they really want those files dropped.")
	}
	return b.String()
}

func orNone(l []string) string {
	if len(l) == 0 {
		return "none"
	}
	return strings.Join(l, ", ")
}

func underTempAny(roots []string) bool {
	for _, r := range roots {
		if underTemp(r) {
			return true
		}
	}
	return false
}
