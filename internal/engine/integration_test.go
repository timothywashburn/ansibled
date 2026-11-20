package engine

// Integration test: the real engine (uv, ansible-core, the ansibled_events
// callback, ansible.posix from Galaxy) against an sshd container.
//
// It is opt-in because it needs docker, uv, ssh-keygen and network access:
//
//	ANSIBLED_INTEGRATION=1 go test -run Integration -v ./internal/engine/
//
// It is skipped with -short, without ANSIBLED_INTEGRATION=1, or when docker is
// not usable. Everything it creates in docker is named ansibled-engine-test-*
// and removed afterwards (the alpine base image too, if the test pulled it).
//
// uv is found through the daemon's PATH: the engine's child PATH is
// <venv>/bin:/usr/local/bin:/usr/bin:/bin followed by the daemon's own PATH
// entries (see basePath), so e.g. /opt/homebrew/bin/uv works on macOS.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/timothywashburn/ansibled/internal/config"
	"github.com/timothywashburn/ansibled/internal/model"
)

const (
	itImage     = "ansibled-engine-test-sshd:latest"
	itBaseImage = "alpine:3.22"
)

const itDockerfile = `FROM ` + itBaseImage + `
RUN apk add --no-cache openssh-server python3 \
 && ssh-keygen -A \
 && sed -i 's/^root:[^:]*:/root:*:/' /etc/shadow \
 && mkdir -p /root/.ssh && chmod 700 /root/.ssh \
 && printf 'greeting = old content\n' > /etc/ansibled-sample.conf
COPY authorized_keys /root/.ssh/authorized_keys
RUN chmod 600 /root/.ssh/authorized_keys
EXPOSE 22
CMD ["/usr/sbin/sshd", "-D", "-e"]
`

func docker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func runCmd(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}

// startSSHD builds the target image, starts it and returns the mapped port.
func startSSHD(t *testing.T, pubKey []byte) string {
	t.Helper()
	ctxDir := t.TempDir()
	os.WriteFile(filepath.Join(ctxDir, "Dockerfile"), []byte(itDockerfile), 0o644)
	os.WriteFile(filepath.Join(ctxDir, "authorized_keys"), pubKey, 0o644)

	hadBase := exec.Command("docker", "image", "inspect", itBaseImage).Run() == nil
	t.Cleanup(func() {
		exec.Command("docker", "rmi", "-f", itImage).Run()
		if !hadBase {
			exec.Command("docker", "rmi", itBaseImage).Run()
		}
	})
	docker(t, "build", "-q", "-t", itImage, ctxDir)

	name := fmt.Sprintf("ansibled-engine-test-sshd-%d", os.Getpid())
	exec.Command("docker", "rm", "-f", name).Run()
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", name).Run() })
	docker(t, "run", "-d", "--name", name, "-p", "127.0.0.1::22", itImage)
	hostPort := docker(t, "port", name, "22/tcp")
	_, port, err := net.SplitHostPort(strings.Split(hostPort, "\n")[0])
	if err != nil {
		t.Fatalf("docker port %q: %v", hostPort, err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", "127.0.0.1:"+port, time.Second)
		if err == nil {
			c.SetDeadline(time.Now().Add(2 * time.Second))
			banner, _ := bufio.NewReader(c).ReadString('\n')
			c.Close()
			if strings.HasPrefix(banner, "SSH-") {
				return port
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("sshd did not come up: %v", err)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// sampleBareRepo commits testdata/sample-repo into a fresh bare repo.
func sampleBareRepo(t *testing.T, dir string) (string, string) {
	t.Helper()
	src, err := filepath.Abs("../../testdata/sample-repo")
	if err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(dir, "work")
	if err := os.CopyFS(work, os.DirFS(src)); err != nil {
		t.Fatal(err)
	}
	os.RemoveAll(filepath.Join(work, ".venv"))
	gitRun(t, work, "init", "-q", "-b", "main")
	gitRun(t, work, "add", "-A")
	gitRun(t, work, "commit", "-q", "-m", "sample")
	sha := gitRun(t, work, "rev-parse", "HEAD")
	bare := filepath.Join(dir, "origin.git")
	gitRun(t, dir, "clone", "-q", "--bare", work, bare)
	return bare, sha
}

func TestIntegration(t *testing.T) {
	if testing.Short() || os.Getenv("ANSIBLED_INTEGRATION") != "1" {
		t.Skip("set ANSIBLED_INTEGRATION=1 to run (needs docker, uv, network)")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skipf("docker not available: %v", err)
	}
	for _, tool := range []string{"uv", "ssh-keygen", "ssh"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not found", tool)
		}
	}

	// A short base directory: ssh's ControlPath (under HOME=<data_dir>/home)
	// must fit in a unix socket path, which macOS's $TMPDIR is too long for.
	base, err := os.MkdirTemp("/tmp", "ansibled-engine-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })

	key := filepath.Join(base, "id_ed25519")
	runCmd(t, base, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "ansibled-test", "-f", key)
	pub, _ := os.ReadFile(key + ".pub")
	port := startSSHD(t, pub)
	bare, sha := sampleBareRepo(t, base)
	cbDir, _ := filepath.Abs("../../callback")

	cfg := &config.Config{
		Site:    "it",
		DataDir: filepath.Join(base, "data"),
		Forge:   config.Forge{CloneURL: bare},
		Repo: config.Repo{
			Workdir:                 ".",
			Setup:                   "uv sync --frozen --no-dev",
			VenvKeyFiles:            []string{"pyproject.toml", "uv.lock"},
			CollectionsRequirements: "requirements.yml",
		},
		Ansible: config.Ansible{
			Inventory:  "inventory.yml",
			Playbooks:  []string{"site.yml"},
			RemoteUser: "root",
			SSHKeyFile: key,
			Timeout:    config.Duration{Duration: 15 * time.Minute},
			Env: map[string]string{
				"SAMPLE_TARGET_HOST":        "127.0.0.1",
				"SAMPLE_TARGET_PORT":        port,
				"ANSIBLE_HOST_KEY_CHECKING": "False",
				// Keep callback/__pycache__ out of the source tree.
				"PYTHONDONTWRITEBYTECODE": "1",
			},
		},
	}
	var mu sync.Mutex
	var groups []int
	e, err := New(Options{Config: cfg, CallbackDir: cbDir, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	e.onStart = func(_ string, pgid int) { mu.Lock(); groups = append(groups, pgid); mu.Unlock() }

	var id int64
	doRun := func(ctx context.Context, extra ...string) (model.State, *model.Summary, string, time.Duration) {
		t.Helper()
		id++
		cfg.Ansible.ExtraArgs = extra
		start := time.Now()
		st, sum := e.Run(ctx, &model.Run{ID: id, Kind: model.KindMain, Ref: "main", SHA: sha})
		b, _ := os.ReadFile(e.LogPath(id))
		return st, sum, string(b), time.Since(start)
	}
	host := func(t *testing.T, sum *model.Summary) model.HostResult {
		t.Helper()
		if len(sum.Playbooks) != 1 || len(sum.Playbooks[0].Hosts) != 1 || sum.Playbooks[0].Hosts[0].Host != "target" {
			t.Fatalf("unexpected summary %+v", sum)
		}
		return sum.Playbooks[0].Hosts[0]
	}

	var firstDur time.Duration
	t.Run("success", func(t *testing.T) {
		st, sum, log, d := doRun(context.Background())
		firstDur = d
		t.Logf("first run took %s", d)
		if testing.Verbose() {
			t.Logf("log.txt:\n%s", log)
		}
		if st != model.StateSuccess {
			t.Fatalf("state %s %+v\n%s", st, sum, log)
		}
		h := host(t, sum)
		if h.Changed < 1 || !slices.Contains(h.ChangedTasks, "Render sample config") {
			t.Errorf("expected a would-change task: %+v\n%s", h, log)
		}
		if h.Ignored != 1 || h.Failed != 0 || h.Unreachable {
			t.Errorf("counts %+v", h)
		}
		if !strings.Contains(log, "ansible.posix") {
			t.Errorf("collection not installed?\n%s", log)
		}
	})

	t.Run("reuses venv", func(t *testing.T) {
		st, sum, log, d := doRun(context.Background())
		t.Logf("second run took %s", d)
		if st != model.StateSuccess {
			t.Fatalf("state %s %+v\n%s", st, sum, log)
		}
		if !strings.Contains(log, "reusing venv") || strings.Contains(log, "==> [setup] uv sync") {
			t.Errorf("venv not reused\n%s", log)
		}
		if !strings.Contains(log, "reusing collection") {
			t.Errorf("collections not reused\n%s", log)
		}
		ents, _ := os.ReadDir(filepath.Join(cfg.DataDir, "venvs"))
		if len(ents) != 1 {
			t.Fatalf("venvs: %v", ents)
		}
		if _, err := os.Stat(filepath.Join(cfg.DataDir, "venvs", ents[0].Name(), markerName)); err != nil {
			t.Error(err)
		}
		if firstDur > 0 && d > firstDur {
			t.Errorf("second run (%s) slower than first (%s)", d, firstDur)
		}
	})

	t.Run("failed", func(t *testing.T) {
		st, sum, log, _ := doRun(context.Background(), "-e", "fail_me=true")
		if st != model.StateFailed {
			t.Fatalf("state %s %+v\n%s", st, sum, log)
		}
		h := host(t, sum)
		if sum.Playbooks[0].ExitCode != 2 || h.Failed != 1 || !slices.Equal(h.FailedTasks, []string{"Fail on request"}) {
			t.Errorf("result %+v exit %d", h, sum.Playbooks[0].ExitCode)
		}
	})

	t.Run("canceled", func(t *testing.T) {
		mu.Lock()
		groups = nil
		mu.Unlock()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		logPath := e.LogPath(id + 1)
		go func() {
			for ctx.Err() == nil {
				time.Sleep(200 * time.Millisecond)
				b, _ := os.ReadFile(logPath)
				if strings.Contains(string(b), "TASK [Slow remote command]") {
					time.Sleep(time.Second) // let ssh get going
					cancel()
				}
			}
		}()
		st, _, log, d := doRun(ctx, "-e", "sleep_seconds=300")
		if st != model.StateCanceled {
			t.Fatalf("state %s\n%s", st, log)
		}
		if testing.Verbose() {
			t.Logf("log.txt:\n%s", log)
		}
		if d > 60*time.Second {
			t.Errorf("cancel took %s", d)
		}
		mu.Lock()
		defer mu.Unlock()
		for _, pg := range groups {
			if err := syscall.Kill(-pg, 0); !errors.Is(err, syscall.ESRCH) {
				out, _ := exec.Command("ps", "-o", "pid,pgid,command", "-g", fmt.Sprint(pg)).CombinedOutput()
				t.Errorf("process group %d still alive:\n%s", pg, out)
			}
		}
		if out, err := exec.Command("pgrep", "-fl", "ansible-playbook.*"+base).CombinedOutput(); err == nil {
			t.Errorf("ansible-playbook still running:\n%s", out)
		}
	})
}
