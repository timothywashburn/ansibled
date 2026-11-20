package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/timothywashburn/ansibled/internal/config"
	"github.com/timothywashburn/ansibled/internal/model"
)

// These tests drive the whole engine against a local git repo whose setup
// command installs a fake ansible-playbook/ansible-galaxy (shell scripts), so
// they need git and sh but no Python, Ansible or network. The real thing is
// exercised by integration_test.go.

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testConfig(dataDir string) *config.Config {
	return &config.Config{
		Site:    "test",
		DataDir: dataDir,
		Repo: config.Repo{
			Workdir:      "ansible",
			Setup:        "sh ./fake-setup.sh",
			VenvKeyFiles: []string{"pyproject.toml", "uv.lock"},
		},
		Ansible: config.Ansible{
			Inventory:  "inventory.yml",
			Playbooks:  []string{"site.yml"},
			SSHKeyFile: "/nonexistent/ssh_key",
			Timeout:    config.Duration{Duration: time.Minute},
			Env:        map[string]string{},
		},
	}
}

const fakeSetup = `#!/bin/sh
set -e
mkdir -p "$ANSIBLED_VENV/bin"
echo "setup ran in $(pwd)"
cp ./fake-ansible-playbook "$ANSIBLED_VENV/bin/ansible-playbook"
cp ./fake-ansible-galaxy "$ANSIBLED_VENV/bin/ansible-galaxy"
chmod +x "$ANSIBLED_VENV/bin/"*
`

// fakePlaybook reads FAKE_MODE (or FAKE_MODE_<playbook basename>) and writes
// canned events.
const fakePlaybook = `#!/bin/sh
pb="$3"
name=$(basename "$pb" .yml)
eval "mode=\${FAKE_MODE_$name:-\${FAKE_MODE:-ok}}"
echo "fake ansible-playbook $* mode=$mode"
echo "--- env"
env | sort
echo "--- end env"
[ -f ../overlay.txt ] && echo "overlay says: $(cat ../overlay.txt)"
ev() { echo "$1" >> "$ANSIBLED_EVENTS_FILE"; }
case "$mode" in
ok)
  ev '{"event":"play_start","playbook":"'$pb'","play":"all"}'
  ev '{"event":"result","status":"ok","host":"h1","task":"ping"}'
  ev '{"event":"result","status":"changed","host":"h1","task":"render config"}'
  ev '{"event":"stats","hosts":{"h1":{"ok":2,"changed":1,"failures":0,"unreachable":0,"skipped":0,"rescued":0,"ignored":0}}}'
  exit 0;;
fail)
  ev '{"event":"result","status":"failed","host":"h1","task":"broken task"}'
  ev '{"event":"stats","hosts":{"h1":{"ok":0,"changed":0,"failures":1,"unreachable":0,"skipped":0,"rescued":0,"ignored":0}}}'
  exit 2;;
exit3)
  ev '{"event":"stats","hosts":{}}'
  exit 3;;
noevents)
  exit 0;;
sleep)
  ev '{"event":"play_start","play":"all"}'
  sleep 60 &
  echo "child $!"
  wait;;
stubborn)
  trap '' TERM
  sleep 60 &
  wait; wait; wait;;
esac
`

const fakeGalaxy = `#!/bin/sh
echo "fake ansible-galaxy $*"
kind="$1"; dir="$6"
mkdir -p "$dir"
echo "$kind $ANSIBLE_COLLECTIONS_PATH" > "$dir/installed"
`

// gitRun runs git in dir with a neutral configuration.
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.org", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.org")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// fakeRepo creates a git repo with an "ansible" workdir and returns its path
// and head sha.
func fakeRepo(t *testing.T, extra map[string]string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"README.md":                     "root\n",
		"ansible/pyproject.toml":        "[project]\nname='x'\n",
		"ansible/uv.lock":               "lock\n",
		"ansible/inventory.yml":         "all: {}\n",
		"ansible/site.yml":              "- hosts: all\n",
		"ansible/other.yml":             "- hosts: all\n",
		"ansible/fake-setup.sh":         fakeSetup,
		"ansible/fake-ansible-playbook": fakePlaybook,
		"ansible/fake-ansible-galaxy":   fakeGalaxy,
	}
	for k, v := range extra {
		files[k] = v
	}
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, dir, "init", "-q", "-b", "main")
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-q", "-m", "init")
	return dir, gitRun(t, dir, "rev-parse", "HEAD")
}

type fakeSource struct{ url, header string }

func (f fakeSource) GitAuth() (string, string) { return f.url, f.header }
func (f fakeSource) MRRef(iid int) string {
	return "refs/merge-requests/" + strconv.Itoa(iid) + "/head"
}

type harness struct {
	t    *testing.T
	e    *Engine
	cfg  *config.Config
	repo string
	sha  string
	mu   sync.Mutex
	pgs  []int
	id   int64
}

func newHarness(t *testing.T, extra map[string]string, mut func(*config.Config)) *harness {
	t.Helper()
	repo, sha := fakeRepo(t, extra)
	cfg := testConfig(t.TempDir())
	cfg.Git.CloneURL = repo
	if mut != nil {
		mut(cfg)
	}
	e, err := New(Options{Config: cfg, CallbackDir: "/cb", Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, e: e, cfg: cfg, repo: repo, sha: sha}
	e.onStart = func(_ string, pgid int) { h.mu.Lock(); h.pgs = append(h.pgs, pgid); h.mu.Unlock() }
	return h
}

func (h *harness) run(ctx context.Context, r *model.Run) (model.State, *model.Summary, string) {
	h.t.Helper()
	h.id++
	if r == nil {
		r = &model.Run{Kind: model.KindMain, Ref: "main", SHA: h.sha}
	}
	r.ID = h.id
	st, sum := h.e.Run(ctx, r)
	log, _ := os.ReadFile(h.e.LogPath(r.ID))
	if _, err := os.Stat(filepath.Join(h.e.RunDir(r.ID), "checkout")); err == nil {
		h.t.Error("checkout/ was not removed")
	}
	return st, sum, string(log)
}

func TestRunSuccessAndReuse(t *testing.T) {
	overlay := t.TempDir()
	os.WriteFile(filepath.Join(overlay, "overlay.txt"), []byte("from overlay"), 0o600)
	os.Symlink("/etc/hosts", filepath.Join(overlay, "badlink"))
	h := newHarness(t, nil, func(c *config.Config) {
		c.OverlayDir = overlay
		c.Ansible.ExtraArgs = []string{"-e", "x=1"}
		c.Ansible.RemoteUser = "ansibled"
		c.Ansible.Env = map[string]string{"FOO": "bar", "ANSIBLE_CALLBACK_PLUGINS": "/mine", "ANSIBLE_CALLBACKS_ENABLED": "timer"}
	})
	st, sum, log := h.run(context.Background(), nil)
	if st != model.StateSuccess {
		t.Fatalf("state %s, summary %+v\n%s", st, sum, log)
	}
	if len(sum.Playbooks) != 1 || sum.Playbooks[0].Playbook != "site.yml" || sum.Playbooks[0].ExitCode != 0 {
		t.Fatalf("summary %+v", sum)
	}
	hr := sum.Playbooks[0].Hosts
	if len(hr) != 1 || hr[0].Changed != 1 || hr[0].ChangedTasks[0] != "render config" {
		t.Errorf("hosts %+v", hr)
	}
	for _, want := range []string{
		"==> [fetch] git -C ",
		"==> [checkout] git -C ",
		"==> [setup] sh ./fake-setup.sh (",
		"setup ran in ",
		"<== [setup] exit 0 in ",
		"warning: overlay: skipping symlink badlink",
		"overlay says: from overlay",
		"==> [ansible] ansible-playbook -i inventory.yml site.yml --check --diff -e x=1 (",
		"\nHOME=" + filepath.Join(h.cfg.DataDir, "home") + "\n",
		"\nLANG=C.UTF-8\n",
		"\nFOO=bar\n",
		"\nANSIBLE_CALLBACK_PLUGINS=/mine:/cb\n",
		"\nANSIBLE_CALLBACKS_ENABLED=timer,ansibled_events\n",
		"\nANSIBLE_PRIVATE_KEY_FILE=/nonexistent/ssh_key\n",
		"\nANSIBLE_REMOTE_USER=ansibled\n",
		"\nANSIBLE_NOCOLOR=1\n",
		"\nANSIBLE_RETRY_FILES_ENABLED=0\n",
		"\nANSIBLED_EVENTS_FILE=" + h.e.EventsPath(1, 0) + "\n",
		"run 1 finished: success",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q", want)
		}
	}
	if !strings.Contains(log, "\nPATH="+filepath.Join(h.cfg.DataDir, "venvs")) {
		t.Error("PATH does not start with the venv")
	}
	if strings.Contains(log, "ANSIBLE_LOG_PATH") {
		t.Error("ANSIBLE_LOG_PATH is set")
	}
	if strings.Contains(log, "GIT_CONFIG") {
		t.Error("git config leaked into ansible env")
	}
	if _, err := os.Stat(h.e.EventsPath(1, 0)); err != nil {
		t.Errorf("events file: %v", err)
	}

	// Second run reuses the venv.
	st, _, log = h.run(context.Background(), nil)
	if st != model.StateSuccess {
		t.Fatalf("second run: %s\n%s", st, log)
	}
	if !strings.Contains(log, "reusing venv") || strings.Contains(log, "setup ran in") {
		t.Errorf("venv not reused:\n%s", log)
	}
}

func TestRunAuthHeaderNotLogged(t *testing.T) {
	repo, sha := fakeRepo(t, nil)
	gitRun(t, repo, "update-ref", "refs/merge-requests/7/head", sha)
	gitRun(t, repo, "checkout", "-q", "-b", "feature")
	os.WriteFile(filepath.Join(repo, "x"), []byte("x"), 0o644)
	gitRun(t, repo, "add", "x")
	gitRun(t, repo, "commit", "-q", "-m", "feature")
	cfg := testConfig(t.TempDir())
	e, err := New(Options{Config: cfg, Source: fakeSource{url: repo, header: "Bearer s3cr3t-t0ken"}, CallbackDir: "/cb", Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	st, sum := e.Run(context.Background(), &model.Run{ID: 1, Kind: model.KindMR, MRIID: 7, Ref: "feature", SHA: sha})
	log, _ := os.ReadFile(e.LogPath(1))
	if st != model.StateSuccess {
		t.Fatalf("state %s %+v\n%s", st, sum, log)
	}
	if !strings.Contains(string(log), "+refs/merge-requests/7/head:refs/merge-requests/7/head") {
		t.Errorf("MR ref not fetched:\n%s", log)
	}
	if strings.Contains(string(log), "s3cr3t") {
		t.Error("auth header in log")
	}
}

func TestRunFailedAndErrors(t *testing.T) {
	h := newHarness(t, nil, func(c *config.Config) {
		c.Ansible.Playbooks = []string{"site.yml", "other.yml"}
	})
	h.cfg.Ansible.Env = map[string]string{"FAKE_MODE_site": "fail"}
	st, sum, log := h.run(context.Background(), nil)
	if st != model.StateFailed {
		t.Fatalf("state %s\n%s", st, log)
	}
	if len(sum.Playbooks) != 2 || sum.Playbooks[0].Hosts[0].FailedTasks[0] != "broken task" || sum.Playbooks[1].ExitCode != 0 {
		t.Errorf("summary %+v", sum)
	}

	h.cfg.Ansible.Env = map[string]string{"FAKE_MODE_site": "exit3"}
	st, sum, log = h.run(context.Background(), nil)
	if st != model.StateError || sum.FailedStep != "ansible" || sum.Error != "site.yml exited 3" {
		t.Fatalf("state %s %+v\n%s", st, sum, log)
	}
	if len(sum.Playbooks) != 2 || sum.Playbooks[1].ExitCode != 0 {
		t.Errorf("second playbook did not run: %+v", sum)
	}

	h.cfg.Ansible.Env = map[string]string{"FAKE_MODE_other": "noevents"}
	st, sum, _ = h.run(context.Background(), nil)
	if st != model.StateError || sum.FailedStep != "ansible" || !strings.Contains(sum.Error, "other.yml: no events") {
		t.Errorf("noevents: %s %+v", st, sum)
	}

	h.cfg.Ansible.Env = nil
	st, sum, _ = h.run(context.Background(), &model.Run{Kind: model.KindMain, Ref: "main", SHA: strings.Repeat("ab", 20)})
	if st != model.StateError || sum.FailedStep != "fetch" || !strings.Contains(sum.Error, "not found") {
		t.Errorf("missing sha: %s %+v", st, sum)
	}
	st, sum, _ = h.run(context.Background(), &model.Run{Kind: model.KindMain, Ref: "nope", SHA: h.sha})
	if st != model.StateError || sum.FailedStep != "fetch" {
		t.Errorf("missing ref: %s %+v", st, sum)
	}
	st, sum, _ = h.run(context.Background(), &model.Run{Kind: model.KindMain, Ref: "main", SHA: "--upload-pack=x"})
	if st != model.StateError || sum.FailedStep != "fetch" {
		t.Errorf("bad sha: %s %+v", st, sum)
	}

	h.cfg.Repo.Setup = "echo broken; exit 1"
	st, sum, log = h.run(context.Background(), nil)
	if st != model.StateError || sum.FailedStep != "setup" || sum.Error != "setup command failed" {
		t.Errorf("setup: %s %+v\n%s", st, sum, log)
	}
	h.cfg.Repo.Setup = "true"
	st, sum, _ = h.run(context.Background(), nil)
	if st != model.StateError || sum.FailedStep != "setup" || !strings.Contains(sum.Error, "ansible-playbook") {
		t.Errorf("setup without ansible: %s %+v", st, sum)
	}
	// Broken setups leave no marker, so they are retried next time.
	ents, _ := os.ReadDir(filepath.Join(h.cfg.DataDir, "venvs"))
	marked := 0
	for _, e := range ents {
		if _, err := os.Stat(filepath.Join(h.cfg.DataDir, "venvs", e.Name(), markerName)); err == nil {
			marked++
		}
	}
	if marked != 1 {
		t.Errorf("%d marked venvs, want 1", marked)
	}
}

func TestRunCollections(t *testing.T) {
	h := newHarness(t, map[string]string{
		"ansible/requirements.yml": "collections:\n  - ansible.posix\nroles:\n  - name: some.role\n",
		"ansible/coll-only.yml":    "collections:\n  - ansible.posix\n",
	}, func(c *config.Config) { c.Repo.CollectionsRequirements = "requirements.yml" })
	st, sum, log := h.run(context.Background(), nil)
	if st != model.StateSuccess {
		t.Fatalf("%s %+v\n%s", st, sum, log)
	}
	hash := fileHash("requirements.yml", []byte("collections:\n  - ansible.posix\nroles:\n  - name: some.role\n"))
	coll := filepath.Join(h.cfg.DataDir, "collections", hash)
	roles := filepath.Join(h.cfg.DataDir, "roles", hash)
	for _, want := range []string{
		"fake ansible-galaxy collection install -r requirements.yml -p " + coll,
		"fake ansible-galaxy role install -r requirements.yml -p " + roles,
		"\nANSIBLE_COLLECTIONS_PATH=" + coll + "\n",
		"/ansible/roles:" + roles + "\n",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q", want)
		}
	}
	for _, d := range []string{coll, roles} {
		if _, err := os.Stat(filepath.Join(d, markerName)); err != nil {
			t.Errorf("marker: %v", err)
		}
	}
	_, _, log = h.run(context.Background(), nil)
	if strings.Contains(log, "fake ansible-galaxy") || !strings.Contains(log, "reusing collection") {
		t.Errorf("collections not reused:\n%s", log)
	}

	h.cfg.Repo.CollectionsRequirements = "coll-only.yml"
	_, _, log = h.run(context.Background(), nil)
	if !strings.Contains(log, "fake ansible-galaxy collection install") || strings.Contains(log, "role install") {
		t.Errorf("collections-only file:\n%s", log)
	}
	if strings.Contains(log, "ANSIBLE_ROLES_PATH") {
		t.Error("ANSIBLE_ROLES_PATH set without roles")
	}

	h.cfg.Repo.CollectionsRequirements = "missing.yml"
	if st, _, _ := h.run(context.Background(), nil); st != model.StateSuccess {
		t.Errorf("missing requirements file: %s", st)
	}
}

func (h *harness) assertNoLeftovers() {
	h.t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, pg := range h.pgs {
		if err := syscall.Kill(-pg, 0); !errors.Is(err, syscall.ESRCH) {
			h.t.Errorf("process group %d still alive (%v)", pg, err)
		}
	}
}

func TestRunCancel(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.cfg.Ansible.Env = map[string]string{"FAKE_MODE": "sleep"}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		// Cancel once ansible-playbook is running.
		for {
			time.Sleep(50 * time.Millisecond)
			b, _ := os.ReadFile(h.e.LogPath(1))
			if strings.Contains(string(b), "child ") {
				cancel()
				return
			}
		}
	}()
	start := time.Now()
	st, _, log := h.run(ctx, nil)
	if st != model.StateCanceled {
		t.Fatalf("state %s\n%s", st, log)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("cancel took %s", d)
	}
	h.assertNoLeftovers()
}

func TestRunCancelEscalatesToKill(t *testing.T) {
	old := killGrace
	killGrace = 500 * time.Millisecond
	defer func() { killGrace = old }()
	h := newHarness(t, nil, nil)
	h.cfg.Ansible.Env = map[string]string{"FAKE_MODE": "stubborn"}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	st, _, log := h.run(ctx, nil)
	if st != model.StateCanceled {
		t.Fatalf("state %s\n%s", st, log)
	}
	h.assertNoLeftovers()
}

func TestRunTimeout(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.cfg.Ansible.Env = map[string]string{"FAKE_MODE": "sleep"}
	h.cfg.Ansible.Timeout.Duration = 2 * time.Second
	st, sum, log := h.run(context.Background(), nil)
	if st != model.StateError || sum.FailedStep != "timeout" {
		t.Fatalf("state %s %+v\n%s", st, sum, log)
	}
	h.assertNoLeftovers()
}

func TestRunWaitsForLock(t *testing.T) {
	h := newHarness(t, nil, nil)
	l, ok, err := tryLock(filepath.Join(h.cfg.DataDir, "engine.lock"))
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	if st, _ := h.e.Run(ctx, &model.Run{ID: 1, Kind: model.KindMain, Ref: "main", SHA: h.sha}); st != model.StateCanceled {
		t.Errorf("state while locked = %s", st)
	}
	// Housekeep skips instead of waiting.
	if err := h.e.Housekeep(0); err != nil {
		t.Error(err)
	}
	l.release()
	if st, _ := h.e.Run(context.Background(), &model.Run{ID: 2, Kind: model.KindMain, Ref: "main", SHA: h.sha}); st != model.StateSuccess {
		t.Errorf("state after unlock = %s", st)
	}
}

func TestNewValidates(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Error("no config accepted")
	}
	if _, err := New(Options{Config: testConfig(t.TempDir())}); err == nil {
		t.Error("no source accepted")
	}
	t.Setenv(CallbackDirEnv, "/from/env")
	cfg := testConfig(t.TempDir())
	cfg.Git.CloneURL = "/x"
	e, err := New(Options{Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	if e.callbackDir != "/from/env" {
		t.Errorf("callback dir %q", e.callbackDir)
	}
	if e.LogPath(5) != filepath.Join(cfg.DataDir, "runs", "5", "log.txt") {
		t.Errorf("LogPath %q", e.LogPath(5))
	}
}
