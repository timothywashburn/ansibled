package e2e

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/timothywashburn/ansibled/internal/forge"
	"github.com/timothywashburn/ansibled/internal/forge/gitlab/gitlabtest"
)

// Everything this test creates in docker starts with prefix.
const (
	prefix       = "ansibled-e2e"
	daemonImage  = prefix + ":test"
	sshdImage    = prefix + "-sshd:test"
	networkName  = prefix + "-net"
	daemonName   = prefix + "-daemon"
	targetName   = prefix + "-target"
	dataVolume   = prefix + "-data"
	sshdBase     = "alpine:3.22"
	hostAlias    = "host.docker.internal"
	containerCfg = "/etc/ansibled"
	containerGit = "/srv/infra.git"
)

// Base images the Containerfile (and the sshd image) pull. Any of them the
// test pulls is removed again afterwards.
var baseImages = []string{"golang:1.26", "ghcr.io/astral-sh/uv:0.9.9", "python:3.13-slim", sshdBase}

const sshdDockerfile = `FROM ` + sshdBase + `
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

// Secrets written into the config directory (test values only).
const (
	site          = "e2e"
	webhookSecret = "e2e-webhook-secret"
	metricsToken  = "e2e-metrics-token"
	sessionKey    = "e2e-session-key-not-a-real-secret-0123456789"
	trustedGroup  = "myorg/sysadmins"
	mrIID         = 1
)

var (
	alice = forge.User{ID: 21, Username: "alice"}
	bob   = forge.User{ID: 22, Username: "bob"}
)

// ---- docker ----

func dockerOut(args ...string) (string, error) {
	out, err := exec.Command("docker", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func docker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := dockerOut(args...)
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func imageExists(name string) bool {
	return exec.Command("docker", "image", "inspect", name).Run() == nil
}

// freePort returns a currently unused loopback TCP port.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// ---- git ----

// gitRepo is a work tree whose commits are pushed into a bare repo that the
// ansibled container fetches from (git.clone_url).
type gitRepo struct {
	t    *testing.T
	work string
	bare string
}

func (g *gitRepo) git(dir string, args ...string) string {
	g.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	// Keep the user's global/system config (signing, hooks, ...) out.
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=e2e", "GIT_AUTHOR_EMAIL=e2e@example.invalid",
		"GIT_COMMITTER_NAME=e2e", "GIT_COMMITTER_EMAIL=e2e@example.invalid",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		g.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newGitRepo commits src as main (M) and returns the repo and M.
func newGitRepo(t *testing.T, base, src string) (*gitRepo, string) {
	t.Helper()
	g := &gitRepo{t: t, work: filepath.Join(base, "work"), bare: filepath.Join(base, "infra.git")}
	if err := os.CopyFS(g.work, os.DirFS(src)); err != nil {
		t.Fatal(err)
	}
	os.RemoveAll(filepath.Join(g.work, ".venv"))
	g.git(g.work, "init", "-q", "-b", "main")
	g.git(g.work, "add", "-A")
	g.git(g.work, "commit", "-q", "-m", "initial")
	g.git(base, "init", "-q", "--bare", g.bare)
	g.git(g.work, "push", "-q", g.bare, "main:refs/heads/main")
	return g, g.git(g.work, "rev-parse", "HEAD")
}

// commit applies edit to the work tree (on the current branch) and commits.
func (g *gitRepo) commit(msg string, edit func(dir string)) string {
	g.t.Helper()
	edit(g.work)
	g.git(g.work, "add", "-A")
	g.git(g.work, "commit", "-q", "-m", msg)
	return g.git(g.work, "rev-parse", "HEAD")
}

// pushMR moves branch and refs/merge-requests/<iid>/head in the bare repo to sha.
func (g *gitRepo) pushMR(branch string, iid int, sha string) {
	g.t.Helper()
	g.git(g.work, "push", "-q", "--force", g.bare,
		sha+":refs/heads/"+branch, fmt.Sprintf("%s:refs/merge-requests/%d/head", sha, iid))
}

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

// replaceInFile replaces exactly one occurrence of old.
func replaceInFile(t *testing.T, path, old, new string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(b), old) != 1 {
		t.Fatalf("%s: expected one %q", path, old)
	}
	writeFile(t, path, strings.Replace(string(b), old, new, 1), 0o644)
}

// ---- HTTP ----

// hostDialer maps host.docker.internal (the fake's address as configured
// for the container) to the loopback address it really listens on, so the
// test's client can follow the same URLs a browser would.
func hostDialer() func(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{Timeout: 5 * time.Second}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if h, p, err := net.SplitHostPort(addr); err == nil && h == hostAlias {
			addr = net.JoinHostPort("127.0.0.1", p)
		}
		return d.DialContext(ctx, network, addr)
	}
}

// newClient returns a client with its own cookie jar; follow=false returns
// redirects instead of following them.
func newClient(follow bool) *http.Client {
	jar, _ := cookiejar.New(nil)
	c := &http.Client{
		Jar:       jar,
		Timeout:   30 * time.Second,
		Transport: &http.Transport{DialContext: hostDialer(), DisableKeepAlives: true},
	}
	if !follow {
		c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}
	return c
}

type response struct {
	Status int
	Header http.Header
	Body   string
	URL    string // final URL after redirects
}

func do(t *testing.T, c *http.Client, req *http.Request) response {
	t.Helper()
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: read body: %v", req.Method, req.URL, err)
	}
	return response{Status: resp.StatusCode, Header: resp.Header, Body: string(b), URL: resp.Request.URL.String()}
}

func get(t *testing.T, c *http.Client, url string, hdr ...string) response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	return do(t, c, req)
}

// sendHook re-targets a request built by gitlabtest (addressed to a dummy
// host) at the daemon's webhook listener and returns the status code.
func sendHook(t *testing.T, webhookURL string, built *http.Request) int {
	t.Helper()
	body, err := io.ReadAll(built.Body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, webhookURL, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header = built.Header.Clone()
	return do(t, newClient(false), req).Status
}

// ---- polling ----

// waitFor polls cond every interval until it returns true or timeout passes.
// cond's string describes the current state for the failure message.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		ok, state := cond()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s; last state: %s", timeout, what, state)
		}
		time.Sleep(time.Second)
	}
}

// ---- fake GitLab observations ----

var runIDRe = regexp.MustCompile(`/runs/(\d+)$`)

// statusesFor returns this site's commit statuses for sha, oldest first.
func statusesFor(fake *gitlabtest.Server, sha string) []gitlabtest.RecordedStatus {
	var out []gitlabtest.RecordedStatus
	for _, s := range fake.Statuses() {
		if s.SHA == sha && s.Name == "ansibled/"+site {
			out = append(out, s)
		}
	}
	return out
}

func runID(s gitlabtest.RecordedStatus) int64 {
	m := runIDRe.FindStringSubmatch(s.TargetURL)
	if m == nil {
		return 0
	}
	id, _ := strconv.ParseInt(m[1], 10, 64)
	return id
}

func statesOf(ss []gitlabtest.RecordedStatus) []string {
	var out []string
	for _, s := range ss {
		out = append(out, string(s.State))
	}
	return out
}

func terminal(s forge.StatusState) bool {
	return s == forge.StatusSuccess || s == forge.StatusFailed || s == forge.StatusCanceled
}

// botNotes returns the MR's notes by the bot that carry this site's marker.
func botNotes(fake *gitlabtest.Server, iid int) []gitlabtest.RecordedNote {
	var out []gitlabtest.RecordedNote
	for _, n := range fake.Notes(iid) {
		if n.Author.ID == gitlabtest.DefaultBotID && strings.Contains(n.Body, "<!-- ansibled:site="+site+" -->") {
			out = append(out, n)
		}
	}
	return out
}

// metricValue finds the first sample of name whose labels include all of
// labels (`k="v"` strings) in a Prometheus text exposition.
func metricValue(body, name string, labels ...string) (float64, bool) {
	sc := bufio.NewScanner(strings.NewReader(body))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, name+"{") && !strings.HasPrefix(line, name+" ") {
			continue
		}
		ok := true
		for _, l := range labels {
			if !strings.Contains(line, l) {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		f := strings.Fields(line)
		v, err := strconv.ParseFloat(f[len(f)-1], 64)
		if err != nil {
			continue
		}
		return v, true
	}
	return 0, false
}

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// newClientWithJar is a non-following client sharing c's cookies.
func newClientWithJar(c *http.Client) *http.Client {
	n := newClient(false)
	n.Jar = c.Jar
	return n
}
