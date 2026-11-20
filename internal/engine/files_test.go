package engine

import (
	"archive/tar"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type tarEntry struct {
	name, body, link string
	typ              byte
	mode             int64
}

func mkTar(t *testing.T, ents ...tarEntry) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range ents {
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: e.mode, Linkname: e.link, Size: int64(len(e.body))}
		if e.typ == tar.TypeXGlobalHeader { // as written by git archive
			h = &tar.Header{Typeflag: e.typ, PAXRecords: map[string]string{"comment": "0123abcd"}}
		}
		if h.Mode == 0 && e.typ != tar.TypeXGlobalHeader {
			h.Mode = 0o644
		}
		if e.typ != tar.TypeReg {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg {
			tw.Write([]byte(e.body))
		}
	}
	tw.Close()
	return &buf
}

func TestExtractTar(t *testing.T) {
	dir := t.TempDir()
	buf := mkTar(t,
		tarEntry{name: "pax_global_header", typ: tar.TypeXGlobalHeader},
		tarEntry{name: "a/", typ: tar.TypeDir, mode: 0o755},
		tarEntry{name: "a/run.sh", typ: tar.TypeReg, body: "#!/bin/sh\n", mode: 0o755},
		tarEntry{name: "b/c/d.txt", typ: tar.TypeReg, body: "deep"},
		tarEntry{name: "link", typ: tar.TypeSymlink, link: "a/run.sh"},
	)
	if err := extractTar(buf, dir); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, "a/run.sh"))
	if err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("run.sh: %v %v", fi, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "b/c/d.txt")); string(b) != "deep" {
		t.Errorf("d.txt = %q", b)
	}
	if l, _ := os.Readlink(filepath.Join(dir, "link")); l != "a/run.sh" {
		t.Errorf("link = %q", l)
	}
}

func TestExtractTarRejectsEscapes(t *testing.T) {
	cases := map[string][]tarEntry{
		"dotdot":   {{name: "../evil", typ: tar.TypeReg, body: "x"}},
		"nested":   {{name: "a/../../evil", typ: tar.TypeReg, body: "x"}},
		"absolute": {{name: "/tmp/evil", typ: tar.TypeReg, body: "x"}},
		"through symlink": {
			{name: "out", typ: tar.TypeSymlink, link: "../outside"},
			{name: "out/evil", typ: tar.TypeReg, body: "x"},
		},
		"through absolute symlink": {
			{name: "out", typ: tar.TypeSymlink, link: "/"},
			{name: "out/evil", typ: tar.TypeReg, body: "x"},
		},
		"device": {{name: "dev", typ: tar.TypeChar}},
	}
	for name, ents := range cases {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			dir := filepath.Join(parent, "checkout")
			os.Mkdir(dir, 0o755)
			os.Mkdir(filepath.Join(parent, "outside"), 0o755)
			if err := extractTar(mkTar(t, ents...), dir); err == nil {
				t.Fatal("expected an error")
			}
			for _, p := range []string{filepath.Join(parent, "evil"), filepath.Join(parent, "outside", "evil"), "/evil", "/tmp/evil"} {
				if _, err := os.Lstat(p); err == nil && !strings.HasPrefix(p, "/tmp") {
					t.Errorf("%s was written", p)
				}
			}
		})
	}
	if _, err := safeRel("../x"); !errors.Is(err, errUnsafePath) {
		t.Errorf("safeRel(../x) = %v", err)
	}
	if c, err := safeRel("./a//b/"); err != nil || c != "a/b" {
		t.Errorf("safeRel(./a//b/) = %q, %v", c, err)
	}
}

func TestCopyOverlay(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(src, "ansible/group_vars"), 0o755))
	must(os.WriteFile(filepath.Join(src, "ansible/group_vars/secret.yml"), []byte("pw: x\n"), 0o600))
	must(os.WriteFile(filepath.Join(src, "ansible/existing.txt"), []byte("overlay"), 0o640))
	must(os.Symlink("/etc/passwd", filepath.Join(src, "ansible/link")))
	must(os.MkdirAll(filepath.Join(dst, "ansible"), 0o755))
	must(os.WriteFile(filepath.Join(dst, "ansible/existing.txt"), []byte("repo"), 0o644))
	must(os.WriteFile(filepath.Join(dst, "ansible/keep.txt"), []byte("keep"), 0o644))

	var warn bytes.Buffer
	n, err := copyOverlay(src, dst, &warn)
	must(err)
	if n != 2 {
		t.Errorf("copied %d files, want 2", n)
	}
	fi, err := os.Stat(filepath.Join(dst, "ansible/group_vars/secret.yml"))
	must(err)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("secret.yml mode %v", fi.Mode())
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "ansible/existing.txt")); string(b) != "overlay" {
		t.Errorf("existing.txt = %q", b)
	}
	if fi, _ := os.Stat(filepath.Join(dst, "ansible/existing.txt")); fi.Mode().Perm() != 0o640 {
		t.Errorf("existing.txt mode %v", fi.Mode())
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "ansible/keep.txt")); string(b) != "keep" {
		t.Errorf("keep.txt = %q", b)
	}
	if _, err := os.Lstat(filepath.Join(dst, "ansible/link")); err == nil {
		t.Error("symlink was copied")
	}
	if !strings.Contains(warn.String(), "skipping symlink ansible/link") {
		t.Errorf("warning missing: %q", warn.String())
	}
}

func TestCopyOverlayStaysInCheckout(t *testing.T) {
	parent := t.TempDir()
	src, dst := filepath.Join(parent, "overlay"), filepath.Join(parent, "checkout")
	os.MkdirAll(filepath.Join(src, "vars"), 0o755)
	os.WriteFile(filepath.Join(src, "vars/x"), []byte("x"), 0o644)
	os.MkdirAll(dst, 0o755)
	os.Mkdir(filepath.Join(parent, "elsewhere"), 0o755)
	// The repo has a symlink pointing out of the checkout at the overlay path.
	os.Symlink("../elsewhere", filepath.Join(dst, "vars"))
	if _, err := copyOverlay(src, dst, &bytes.Buffer{}); err == nil {
		t.Fatal("expected an error")
	}
	if _, err := os.Stat(filepath.Join(parent, "elsewhere/x")); err == nil {
		t.Fatal("overlay wrote outside the checkout")
	}
}

func TestVenvHash(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte("a"), 0o644)
	os.WriteFile(filepath.Join(dir, "uv.lock"), []byte("b"), 0o644)
	keys := []string{"pyproject.toml", "uv.lock"}
	h := func(setup string, keys []string) string {
		t.Helper()
		s, err := venvHash(dir, setup, keys)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	base := h("uv sync", keys)
	if len(base) != 64 {
		t.Fatalf("hash %q", base)
	}
	if h("uv sync", keys) != base {
		t.Error("hash not stable")
	}
	if h("uv sync --frozen", keys) == base {
		t.Error("setup command not hashed")
	}
	if h("uv sync", []string{"uv.lock", "pyproject.toml"}) == base {
		t.Error("order not hashed")
	}
	missing := h("uv sync", append(keys, "requirements.txt"))
	if missing == base {
		t.Error("missing file not hashed")
	}
	os.WriteFile(filepath.Join(dir, "requirements.txt"), []byte("<missing>"), 0o644)
	if h("uv sync", append(keys, "requirements.txt")) == missing {
		t.Error("missing file and file containing <missing> collide")
	}
	os.WriteFile(filepath.Join(dir, "uv.lock"), []byte("c"), 0o644)
	if h("uv sync", keys) == base {
		t.Error("content not hashed")
	}
	// Moving bytes between path and content must change the hash.
	if fileHash("ab", []byte("c")) == fileHash("a", []byte("bc")) {
		t.Error("fileHash ambiguous")
	}
}

func TestRequirementsKinds(t *testing.T) {
	cases := []struct {
		in         string
		coll, role bool
		err        bool
	}{
		{"collections:\n  - ansible.posix\n", true, false, false},
		{"roles:\n  - name: x\ncollections: []\n", true, true, false},
		{"roles:\n  - name: x\n", false, true, false},
		{"- src: geerlingguy.docker\n", false, true, false},
		{"", false, false, false},
		{"just a string", false, false, true},
		{"{{", false, false, true},
	}
	for _, c := range cases {
		coll, role, err := requirementsKinds([]byte(c.in))
		if coll != c.coll || role != c.role || (err != nil) != c.err {
			t.Errorf("%q: got %v %v %v", c.in, coll, role, err)
		}
	}
}

func TestBasePath(t *testing.T) {
	got := basePath("/data/venvs/x", "/opt/homebrew/bin:/usr/bin:relative::/usr/local/sbin")
	want := "/data/venvs/x/bin:/usr/local/bin:/usr/bin:/bin:/opt/homebrew/bin:/usr/local/sbin"
	if got != want {
		t.Errorf("basePath = %q, want %q", got, want)
	}
}

func TestHousekeep(t *testing.T) {
	dir := t.TempDir()
	e := &Engine{cfg: testConfig(dir), log: discardLogger()}
	old := time.Now().Add(-30 * 24 * time.Hour)
	mk := func(rel string, marker bool, mtime time.Time) string {
		p := filepath.Join(dir, rel)
		os.MkdirAll(p, 0o755)
		if marker {
			os.WriteFile(filepath.Join(p, markerName), nil, 0o644)
			os.Chtimes(filepath.Join(p, markerName), mtime, mtime)
		}
		os.Chtimes(p, mtime, mtime)
		return p
	}
	stale := mk("venvs/stale", true, old)
	fresh := mk("venvs/fresh", true, time.Now())
	partialOld := mk("collections/partial", false, old)
	partialNew := mk("roles/partial", false, time.Now())
	// A fresh marker inside an old directory counts as used.
	used := mk("roles/used", false, old)
	os.WriteFile(filepath.Join(used, markerName), nil, 0o644)

	if err := e.Housekeep(14 * 24 * time.Hour); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]bool{stale: false, fresh: true, partialOld: false, partialNew: true, used: true} {
		_, err := os.Stat(p)
		if (err == nil) != want {
			t.Errorf("%s exists=%v, want %v", p, err == nil, want)
		}
	}
	if err := e.DeleteRunFiles(42); err != nil {
		t.Errorf("DeleteRunFiles on missing dir: %v", err)
	}
}
