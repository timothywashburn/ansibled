package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// passThroughNames are copied from the daemon's environment into every child
// process, in addition to everything starting with "UV_". They configure
// network access and uv, and carry no ansibled secrets.
var passThroughNames = map[string]bool{
	"HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true,
	"http_proxy": true, "https_proxy": true, "no_proxy": true,
	"SSL_CERT_FILE": true, "SSL_CERT_DIR": true, "REQUESTS_CA_BUNDLE": true,
	"TZ": true,
}

func passThroughEnv(environ []string) []string {
	var out []string
	for _, kv := range environ {
		k, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if passThroughNames[k] || strings.HasPrefix(k, "UV_") {
			out = append(out, kv)
		}
	}
	return out
}

// basePath builds the child PATH: the venv's bin (if any), the standard
// system directories, then the daemon's own PATH entries (so uv, git and sh
// are found where they are installed, e.g. /opt/homebrew/bin), without
// duplicates.
func basePath(venv, daemonPath string) string {
	var parts []string
	seen := map[string]bool{}
	add := func(p string) {
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		parts = append(parts, p)
	}
	if venv != "" {
		add(filepath.Join(venv, "bin"))
	}
	for _, p := range []string{"/usr/local/bin", "/usr/bin", "/bin"} {
		add(p)
	}
	for _, p := range filepath.SplitList(daemonPath) {
		if filepath.IsAbs(p) {
			add(p)
		}
	}
	return strings.Join(parts, string(os.PathListSeparator))
}

// env is an ordered set of environment variables; later Set calls win.
type env struct {
	keys []string
	vals map[string]string
}

func newEnv() *env { return &env{vals: map[string]string{}} }

func (e *env) Set(k, v string) {
	if _, ok := e.vals[k]; !ok {
		e.keys = append(e.keys, k)
	}
	e.vals[k] = v
}

func (e *env) Get(k string) (string, bool) { v, ok := e.vals[k]; return v, ok }

func (e *env) SetAll(kvs []string) {
	for _, kv := range kvs {
		if k, v, ok := strings.Cut(kv, "="); ok {
			e.Set(k, v)
		}
	}
}

func (e *env) Environ() []string {
	out := make([]string, 0, len(e.keys))
	for _, k := range e.keys {
		out = append(out, k+"="+e.vals[k])
	}
	return out
}

// baseEnv is the minimal environment every child gets (not the daemon's own).
func (rc *runCtxt) baseEnv(venv string) *env {
	ev := newEnv()
	ev.SetAll(rc.passEnv)
	ev.Set("PATH", basePath(venv, rc.daemonPath))
	ev.Set("HOME", rc.homeDir())
	ev.Set("LANG", "C.UTF-8")
	return ev
}

// step describes one child process.
type step struct {
	name    string   // "fetch", "setup", ... (log header and slog)
	display string   // command line shown in the log; argv if empty
	argv    []string // argv[0] is resolved against the child PATH
	dir     string
	env     *env
	stdin   io.Reader
	stdout  io.Writer // default: the run log (combined with stderr)
	quiet   bool      // no header/footer in the log
}

// exec runs a step, writing its output to the run log framed by a header and
// a footer. It returns the exit code (-1 if the process could not run or was
// killed by a signal) and an error if it did not exit 0.
func (rc *runCtxt) exec(s step) (int, error) {
	start := time.Now()
	display := s.display
	if display == "" {
		display = strings.Join(s.argv, " ")
	}
	if !s.quiet {
		fmt.Fprintf(rc.log, "\n==> [%s] %s (%s)\n", s.name, display, stamp(start))
	}
	code, err := runProcess(rc.ctx, s, rc.log, rc.onStart)
	if !s.quiet {
		fmt.Fprintf(rc.log, "<== [%s] exit %d in %s\n", s.name, code, time.Since(start).Round(time.Millisecond))
	}
	rc.lg.Debug("step finished", "step", s.name, "exit", code, "duration", time.Since(start).Round(time.Millisecond))
	return code, err
}

// runProcess starts the process in its own process group and waits for it.
// When ctx is done the whole group gets SIGTERM, and SIGKILL if anything in it
// is still alive killGrace later.
func runProcess(ctx context.Context, s step, logw io.Writer, onStart func(string, int)) (int, error) {
	if err := ctx.Err(); err != nil {
		return -1, err
	}
	path := ""
	if p, ok := s.env.Get("PATH"); ok {
		path = p
	}
	bin, err := lookPath(s.argv[0], path)
	if err != nil {
		fmt.Fprintf(logw, "%v\n", err)
		return -1, err
	}
	cmd := exec.Command(bin, s.argv[1:]...)
	cmd.Args[0] = s.argv[0]
	cmd.Dir = s.dir
	cmd.Env = s.env.Environ()
	cmd.Stdin = s.stdin
	out := s.stdout
	if out == nil {
		out = logw
	}
	cmd.Stdout = out
	cmd.Stderr = logw
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(logw, "cannot start %s: %v\n", s.argv[0], err)
		return -1, err
	}
	pgid := cmd.Process.Pid
	if onStart != nil {
		onStart(s.name, pgid)
	}

	waited := make(chan struct{})
	killed := make(chan struct{})
	go func() {
		defer close(killed)
		select {
		case <-waited:
			return
		case <-ctx.Done():
		}
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
		t := time.NewTimer(killGrace)
		defer t.Stop()
		// Wait for the leader, then for the rest of the group, up to killGrace.
		select {
		case <-waited:
		case <-t.C:
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			return
		}
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for groupAlive(pgid) {
			select {
			case <-t.C:
				_ = syscall.Kill(-pgid, syscall.SIGKILL)
				return
			case <-tick.C:
			}
		}
	}()
	werr := cmd.Wait()
	close(waited)
	<-killed

	code := -1
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	if ctx.Err() != nil {
		return code, ctx.Err()
	}
	if werr != nil {
		var ee *exec.ExitError
		if errors.As(werr, &ee) {
			return code, fmt.Errorf("%s exited %d", s.argv[0], code)
		}
		return code, werr
	}
	return code, nil
}

// groupAlive reports whether any process is left in process group pgid.
func groupAlive(pgid int) bool {
	return !errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH)
}

// lookPath resolves name against path (the child's PATH, not the daemon's).
func lookPath(name, path string) (string, error) {
	if strings.Contains(name, "/") {
		return name, nil
	}
	for _, dir := range filepath.SplitList(path) {
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s: not found in PATH", name)
}

// sortedKeys returns m's keys in order (stable env output).
func sortedKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
