package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

func readJournal(dir string) ([]jrec, error) {
	f, err := os.Open(filepath.Join(dir, "undo.jsonl"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []jrec
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var r jrec
		if json.Unmarshal(sc.Bytes(), &r) == nil {
			out = append(out, r)
		}
	}
	return out, sc.Err()
}

func runUndo(j *Job) error {
	target := j.Spec.UndoOf
	tdir := filepath.Join(jobsDir(), target)
	if v, ok := running.Load(target); ok {
		_ = v
		return errors.New("that job is still running; cancel it first")
	}
	recs, err := readJournal(tdir)
	if err != nil || len(recs) == 0 {
		return errors.New("nothing to undo for that job (it was a dry run or changed nothing)")
	}
	if b, err := os.ReadFile(filepath.Join(tdir, "undone")); err == nil {
		return fmt.Errorf("already undone by job %s", string(b))
	}
	res := j.newStep("undo " + target)
	dry := j.Spec.DryRun
	var pending []string
	flush := func() {
		if len(pending) > 0 && !dry {
			j.discardInto(res, pending, "removed copies")
		}
		pending = nil
	}
	j.addTotals(int64(len(recs)), 0)
	for i := len(recs) - 1; i >= 0; i-- {
		if j.ctx.Err() != nil {
			return errCanceled
		}
		r := recs[i]
		switch r.Op {
		case "move", "rename":
			flush()
			if !exists(r.To) {
				res.err("no longer there: " + r.To)
				break
			}
			if exists(r.From) && !samePath(r.From, r.To) {
				res.err("original spot is taken again: " + r.From)
				break
			}
			if dry {
				res.add("would put back", 1)
				if len(res.Samples) < 6 {
					res.sample(r.To + " → " + r.From)
				}
				break
			}
			if err := moveBack(j, r.To, r.From); err != nil {
				res.err(fmt.Sprintf("%s: %s", r.To, cleanErr(err)))
				break
			}
			res.add("put back", 1)
		case "copy":
			fi, err := os.Stat(r.Path)
			if err != nil {
				break
			}
			if fi.Size() != r.Size || fi.ModTime().UnixNano() != r.MTime {
				res.add("left alone (changed since)", 1)
				break
			}
			if dry {
				res.add("would remove copy (to Trash)", 1)
				break
			}
			pending = append(pending, r.Path)
		case "edit":
			flush()
			fi, err := os.Stat(r.Path)
			if err != nil {
				res.err("no longer there: " + r.Path)
				break
			}
			same := fi.Size() == r.Size && fi.ModTime().UnixNano() == r.MTime
			if !same && r.Hash != "" && fi.Size() == r.Size {
				h, _ := fileSHA256(r.Path)
				same = h == r.Hash
			}
			if !same {
				res.add("left alone (changed since)", 1)
				break
			}
			if dry {
				res.add("would restore previous version", 1)
				break
			}
			tmp := r.Path + ".csundo"
			if err := copyPlain(r.Held, tmp); err != nil {
				res.err(fmt.Sprintf("%s: %s", r.Path, cleanErr(err)))
				break
			}
			if err := os.Rename(tmp, r.Path); err != nil {
				os.Remove(tmp)
				res.err(fmt.Sprintf("%s: %s", r.Path, cleanErr(err)))
				break
			}
			res.add("restored previous version", 1)
		case "discard":
			flush()
			if exists(r.Path) {
				res.err("can't restore, something is there now: " + r.Path)
				break
			}
			if dry {
				res.add("would restore", 1)
				if len(res.Samples) < 6 {
					res.sample("restore " + r.Path)
				}
				break
			}
			var err error
			if r.Method == "hold" {
				if err = os.MkdirAll(filepath.Dir(r.Path), 0o755); err == nil {
					err = os.Rename(r.Held, r.Path)
				}
			} else {
				err = restoreRecycled(r.Path, time.Unix(0, r.At))
			}
			if err != nil {
				res.err(fmt.Sprintf("%s: %s", r.Path, cleanErr(err)))
				break
			}
			res.add("restored", 1)
		case "mkdir":
			flush()
			if !dry {
				os.Remove(r.Path)
			}
		case "rmdir":
			flush()
			if !dry {
				os.MkdirAll(r.Path, 0o755)
			}
		}
		j.fileDone()
	}
	flush()
	if !dry {
		os.WriteFile(filepath.Join(tdir, "undone"), []byte(j.ID), 0o644)
	}
	return nil
}

func moveBack(j *Job, from, to string) error {
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	err := os.Rename(from, to)
	if err == nil || !isCrossDevice(err) {
		return err
	}
	return moveAcross(j, from, to)
}

// moveAcross moves a file or folder to another drive: copy, then remove the source.
func moveAcross(j *Job, from, to string) error {
	fi, err := os.Stat(from)
	if err != nil {
		return err
	}
	if fi.IsDir() {
		return moveTree(from, to)
	}
	if err := copyFileData(j, from, to, "size", true); err != nil {
		return err
	}
	return os.Remove(from)
}
