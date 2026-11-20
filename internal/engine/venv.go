package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

var timeNow = time.Now

// venvHash is sha256 over the setup command and, for each key file (relative
// to workdir, in the configured order), its path and its content or
// "<missing>". Fields are separated by NUL bytes so they cannot run together.
func venvHash(workdir, setup string, keyFiles []string) (string, error) {
	h := sha256.New()
	fmt.Fprintf(h, "setup\x00%s\x00", setup)
	for _, f := range keyFiles {
		fmt.Fprintf(h, "file\x00%s\x00", filepath.ToSlash(f))
		b, err := os.ReadFile(filepath.Join(workdir, filepath.FromSlash(f)))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			h.Write([]byte("<missing>"))
		case err != nil:
			return "", err
		default:
			fmt.Fprintf(h, "%d\x00", len(b))
			h.Write(b)
		}
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// fileHash is sha256 over a path and the file's content (collections).
func fileHash(rel string, content []byte) string {
	h := sha256.New()
	fmt.Fprintf(h, "file\x00%s\x00%d\x00", filepath.ToSlash(rel), len(content))
	h.Write(content)
	return hex.EncodeToString(h.Sum(nil))
}

// markReady reports whether dir carries the marker, refreshing its mtime if so.
func markReady(dir string) bool {
	m := filepath.Join(dir, markerName)
	if fi, err := os.Stat(m); err != nil || !fi.Mode().IsRegular() {
		return false
	}
	now := timeNow()
	_ = os.Chtimes(m, now, now)
	return true
}

// writeMarker marks dir as completely installed.
func writeMarker(dir, what string) error {
	return os.WriteFile(filepath.Join(dir, markerName), []byte(what+"\n"+stamp(timeNow())+"\n"), 0o644)
}

// setupVenv is step 4: find or create the Python environment.
func (rc *runCtxt) setupVenv() error {
	wd := rc.workdir()
	if fi, err := os.Stat(wd); err != nil || !fi.IsDir() {
		return stepErr("setup", "workdir %s not found in the checkout", rc.cfg.Repo.Workdir)
	}
	hash, err := venvHash(wd, rc.cfg.Repo.Setup, rc.cfg.Repo.VenvKeyFiles)
	if err != nil {
		return stepErr("setup", "cannot hash venv key files")
	}
	venv := filepath.Join(rc.cfg.DataDir, "venvs", hash)
	rc.venv = venv
	if markReady(venv) {
		fmt.Fprintf(rc.log, "\n==> [setup] reusing venv %s\n", venv)
		return nil
	}
	if err := os.RemoveAll(venv); err != nil {
		return stepErr("setup", "cannot remove partial venv")
	}
	if err := os.MkdirAll(filepath.Dir(venv), 0o755); err != nil {
		return stepErr("setup", "cannot create venvs directory")
	}
	cache := filepath.Join(rc.cfg.DataDir, "cache", "uv")
	if err := os.MkdirAll(cache, 0o755); err != nil {
		return stepErr("setup", "cannot create uv cache directory")
	}
	ev := rc.baseEnv(venv)
	ev.Set("ANSIBLED_VENV", venv)
	ev.Set("UV_PROJECT_ENVIRONMENT", venv)
	ev.Set("UV_CACHE_DIR", cache)
	_, err = rc.exec(step{
		name:    "setup",
		display: rc.cfg.Repo.Setup,
		argv:    []string{"sh", "-c", rc.cfg.Repo.Setup},
		dir:     wd,
		env:     ev,
	})
	if err != nil {
		if rc.ctx.Err() != nil {
			return rc.ctx.Err()
		}
		return stepErr("setup", "setup command failed")
	}
	if fi, err := os.Stat(filepath.Join(venv, "bin", "ansible-playbook")); err != nil || fi.IsDir() {
		fmt.Fprintf(rc.log, "%s/bin/ansible-playbook does not exist after setup\n", venv)
		return stepErr("setup", "setup did not install ansible-playbook into the venv")
	}
	if err := writeMarker(venv, rc.cfg.Repo.Setup); err != nil {
		return stepErr("setup", "cannot write venv marker")
	}
	return nil
}

// requirementsKinds reports whether a requirements file lists collections
// and/or roles. The old format (a bare list) is roles only.
func requirementsKinds(b []byte) (collections, roles bool, err error) {
	var n yaml.Node
	if err := yaml.Unmarshal(b, &n); err != nil {
		return false, false, err
	}
	if len(n.Content) == 0 {
		return false, false, nil
	}
	doc := n.Content[0]
	switch doc.Kind {
	case yaml.SequenceNode:
		return false, len(doc.Content) > 0, nil
	case yaml.MappingNode:
		for i := 0; i+1 < len(doc.Content); i += 2 {
			switch doc.Content[i].Value {
			case "collections":
				collections = true
			case "roles":
				roles = true
			}
		}
		return collections, roles, nil
	default:
		return false, false, fmt.Errorf("unexpected YAML %s", doc.Tag)
	}
}

// setupCollections is step 5: install collections and roles from
// repo.collections_requirements, if that file exists.
func (rc *runCtxt) setupCollections() error {
	rel := rc.cfg.Repo.CollectionsRequirements
	if rel == "" {
		return nil
	}
	file := filepath.Join(rc.workdir(), filepath.FromSlash(rel))
	b, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return stepErr("collections", "cannot read %s", rel)
	}
	wantColl, wantRoles, err := requirementsKinds(b)
	if err != nil {
		fmt.Fprintf(rc.log, "%s: %v\n", rel, err)
		return stepErr("collections", "cannot parse %s", rel)
	}
	hash := fileHash(rel, b)
	install := func(kind, dir string) error {
		if markReady(dir) {
			fmt.Fprintf(rc.log, "\n==> [collections] reusing %s %s\n", kind, dir)
			return nil
		}
		if err := os.RemoveAll(dir); err != nil {
			return stepErr("collections", "cannot remove partial %s directory", kind)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return stepErr("collections", "cannot create %s directory", kind)
		}
		ev := rc.ansibleEnv()
		if kind == "collection" {
			ev.Set("ANSIBLE_COLLECTIONS_PATH", dir)
		}
		argv := []string{filepath.Join(rc.venv, "bin", "ansible-galaxy"), kind, "install", "-r", rel, "-p", dir}
		if _, err := rc.exec(step{name: "collections", argv: argv, dir: rc.workdir(), env: ev}); err != nil {
			if rc.ctx.Err() != nil {
				return rc.ctx.Err()
			}
			return stepErr("collections", "ansible-galaxy %s install failed", kind)
		}
		if err := writeMarker(dir, kind+" "+rel); err != nil {
			return stepErr("collections", "cannot write marker")
		}
		return nil
	}
	if wantColl {
		dir := filepath.Join(rc.cfg.DataDir, "collections", hash)
		if err := install("collection", dir); err != nil {
			return err
		}
		rc.collections = dir
	}
	if wantRoles {
		dir := filepath.Join(rc.cfg.DataDir, "roles", hash)
		if err := install("role", dir); err != nil {
			return err
		}
		rc.roles = dir
	}
	return nil
}

// housekeepDirs are the cache directories Housekeep prunes.
var housekeepDirs = []string{"venvs", "collections", "roles"}

// Housekeep deletes venvs, collections and roles whose marker was not touched
// for unusedFor (and partial installs older than that). It skips everything
// if a run currently holds the engine lock.
func (e *Engine) Housekeep(unusedFor time.Duration) error {
	lock, ok, err := tryLock(filepath.Join(e.cfg.DataDir, "engine.lock"))
	if err != nil {
		return err
	}
	if !ok {
		e.log.Info("housekeeping skipped, a run is in progress")
		return nil
	}
	defer lock.release()
	cutoff := timeNow().Add(-unusedFor)
	var errs []error
	for _, d := range housekeepDirs {
		base := filepath.Join(e.cfg.DataDir, d)
		ents, err := os.ReadDir(base)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, ent := range ents {
			if !ent.IsDir() || strings.HasPrefix(ent.Name(), ".") {
				continue
			}
			dir := filepath.Join(base, ent.Name())
			fi, err := os.Stat(filepath.Join(dir, markerName))
			if err != nil {
				fi, err = os.Stat(dir) // partial install: judge by the directory
			}
			if err != nil || fi.ModTime().After(cutoff) {
				continue
			}
			e.log.Info("removing unused cache directory", "dir", dir, "last_used", fi.ModTime())
			if err := os.RemoveAll(dir); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}
