package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type SavedJob struct {
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Steps       []Step    `json:"steps"`
	Created     time.Time `json:"created"`
}

func slug(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		case r == ' ' || r == '.':
			b.WriteRune('-')
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		s = "job-" + randHex(3)
	}
	return clip(s, 60)
}

func validName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 80 {
		return errors.New("give the saved job a name of 1 to 80 characters")
	}
	if strings.ContainsAny(name, `"%^&|<>\/:*?`) {
		return errors.New(`the name can't contain any of " % ^ & | < > \ / : * ?`)
	}
	return nil
}

func listSaved() []SavedJob {
	ents, _ := os.ReadDir(savedDir())
	var out []SavedJob
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(savedDir(), e.Name()))
		if err != nil {
			continue
		}
		var s SavedJob
		if json.Unmarshal(b, &s) == nil && s.Name != "" {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(a, b int) bool { return strings.ToLower(out[a].Name) < strings.ToLower(out[b].Name) })
	return out
}

func findSavedPath(name string) (string, *SavedJob) {
	for _, s := range listSaved() {
		if strings.EqualFold(strings.TrimSpace(s.Name), strings.TrimSpace(name)) {
			s := s
			return filepath.Join(savedDir(), slug(s.Name)+".json"), &s
		}
	}
	return "", nil
}

func loadSaved(name string) (*SavedJob, error) {
	if _, s := findSavedPath(name); s != nil {
		return s, nil
	}
	return nil, fmt.Errorf("no saved job called %q", name)
}

func saveJob(s SavedJob) (string, error) {
	if err := validName(s.Name); err != nil {
		return "", err
	}
	if err := validateSteps(s.Steps); err != nil {
		return "", err
	}
	s.Name = strings.TrimSpace(s.Name)
	if old, prev := findSavedPath(s.Name); prev != nil {
		os.Remove(old)
	}
	s.Created = time.Now()
	if err := writeJSONAtomic(filepath.Join(savedDir(), slug(s.Name)+".json"), s); err != nil {
		return "", err
	}
	return writeShortcut(s.Name)
}

func removeSaved(name string) error {
	p, s := findSavedPath(name)
	if s == nil {
		return fmt.Errorf("no saved job called %q", name)
	}
	os.Remove(p)
	os.Remove(shortcutPath(s.Name))
	return nil
}

func jobsFolder() string { return filepath.Join(appRoot(), "Saved Jobs") }

func shortcutPath(name string) string { return filepath.Join(jobsFolder(), name+".desktop") }

// writeShortcut: a launcher in Saved Jobs that re-runs the job on double-click.
func writeShortcut(name string) (string, error) {
	exe := installPath()
	if !exists(exe) {
		exe, _ = os.Executable()
	}
	p := shortcutPath(name)
	return p, writeLauncher(p, name, "Run the saved ContextSaver job "+name, "", []string{exe, "run", name}, true, 0)
}

func describeSteps(steps []Step) string {
	var parts []string
	for i := range steps {
		parts = append(parts, stepTitle(&steps[i]))
	}
	return strings.Join(parts, "; ")
}

func hasRisky(steps []Step) bool {
	for _, s := range steps {
		switch strings.ToLower(s.Op) {
		case "delete", "rename", "edit", "write", "extract":
			return true
		case "sync":
			if s.DeleteExtra {
				return true
			}
		}
		if c := strings.ToLower(s.Conflict); c == "overwrite" || c == "newer" {
			return true
		}
	}
	return false
}
