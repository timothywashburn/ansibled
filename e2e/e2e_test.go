// Package e2e is ansibled's end-to-end test: the real container image, a fake
// GitLab (internal/forge/gitlab/gitlabtest) and a fake OpenID Connect
// provider (internal/oidc/oidctest) on the host, a bare git repo mounted into
// the container and an sshd container as the managed host.
//
// It is opt-in because it needs docker and network access (uv, PyPI, Galaxy):
//
//	ANSIBLED_E2E=1 go test -v -timeout 30m ./e2e/
//
// It is written for Docker Desktop (the container reaches the host's fakes
// as host.docker.internal). Everything it creates in docker is named
// ansibled-e2e* and removed afterwards, including base images it pulled.
package e2e

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/timothywashburn/ansibled/internal/forge"
	"github.com/timothywashburn/ansibled/internal/forge/gitlab/gitlabtest"
	"github.com/timothywashburn/ansibled/internal/oidc/oidctest"
)

// suite is the shared state of the scenarios, which run in order.
type suite struct {
	t    *testing.T
	fake *gitlabtest.Server
	idp  *oidctest.Server
	repo *gitRepo

	cfgDir     string
	webURL     string // public_url
	webhookURL string
	forgeURL   string // as the container sees it
	issuer     string // OIDC issuer, as the container (and the browser) sees it
	runArgs    []string

	main, feature string // M and F
	head          string // current MR head
	mainRunID     int64
	noteID        int64
	alice         *http.Client // signed-in browser (after the web UI step)
	lastRunID     int64
}

func TestE2E(t *testing.T) {
	if testing.Short() || os.Getenv("ANSIBLED_E2E") != "1" {
		t.Skip("set ANSIBLED_E2E=1 to run (needs docker and network access)")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skipf("docker not available: %v", err)
	}
	for _, tool := range []string{"git", "ssh-keygen"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not found", tool)
		}
	}

	s := &suite{t: t}
	start := time.Now()
	s.setup()
	t.Logf("setup took %s", time.Since(start).Round(time.Second))

	steps := []struct {
		name string
		fn   func(t *testing.T)
	}{
		{"a_startup_catch_up", s.startupCatchUp},
		{"b_webhook_auth", s.webhookAuth},
		{"c_trusted_push", s.trustedPush},
		{"d_untrusted_push", s.untrustedPush},
		{"e_check_comment", s.checkComment},
		{"f_failure", s.failure},
		{"g_metrics", s.metrics},
		{"h_web_ui", s.webUI},
		{"i_restart", s.restart},
		{"j_cancel_on_merge", s.cancelOnMerge},
	}
	for _, st := range steps {
		t0 := time.Now()
		ok := t.Run(st.name, st.fn)
		t.Logf("%s took %s", st.name, time.Since(t0).Round(100*time.Millisecond))
		if !ok {
			t.Fatalf("%s failed; skipping the remaining scenarios", st.name)
		}
	}
	t.Logf("total %s", time.Since(start).Round(time.Second))
}

// ---------------------------------------------------------------------------
// setup

func (s *suite) setup() {
	t := s.t
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()

	// Images: remember which base images exist so the ones we pull are removed.
	had := map[string]bool{}
	for _, img := range baseImages {
		had[img] = imageExists(img)
	}
	t.Cleanup(func() {
		for _, img := range []string{daemonImage, sshdImage} {
			exec.Command("docker", "rmi", "-f", img).Run()
		}
		for _, img := range baseImages {
			if !had[img] && imageExists(img) {
				exec.Command("docker", "rmi", img).Run()
			}
		}
	})
	removeContainers := func() {
		exec.Command("docker", "rm", "-f", daemonName, targetName).Run()
		exec.Command("docker", "volume", "rm", "-f", dataVolume).Run()
		exec.Command("docker", "network", "rm", networkName).Run()
	}
	removeContainers() // leftovers of an interrupted run
	t.Cleanup(removeContainers)

	t0 := time.Now()
	docker(t, "build", "-q", "--build-arg", "VERSION=e2e", "-f", filepath.Join(root, "Containerfile"), "-t", daemonImage, root)
	t.Logf("image build took %s", time.Since(t0).Round(time.Second))

	// SSH key and target.
	keyDir := filepath.Join(base, "key")
	os.MkdirAll(keyDir, 0o700)
	key := filepath.Join(keyDir, "id_ed25519")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "ansibled-e2e", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, out)
	}
	pub, _ := os.ReadFile(key + ".pub")
	priv, _ := os.ReadFile(key)
	sshdCtx := filepath.Join(base, "sshd")
	writeFile(t, filepath.Join(sshdCtx, "Dockerfile"), sshdDockerfile, 0o644)
	writeFile(t, filepath.Join(sshdCtx, "authorized_keys"), string(pub), 0o644)
	docker(t, "build", "-q", "-t", sshdImage, sshdCtx)
	docker(t, "network", "create", networkName)
	docker(t, "run", "-d", "--name", targetName, "--network", networkName, sshdImage)

	// Repo: main at M, feature at F, refs/merge-requests/1/head -> F.
	src, _ := filepath.Abs("../testdata/sample-repo")
	g, m := newGitRepo(t, base, src)
	s.repo, s.main = g, m
	g.git(g.work, "checkout", "-q", "-b", "feature")
	s.feature = g.commit("feature: add notes", func(dir string) {
		writeFile(t, filepath.Join(dir, "NOTES.md"), "feature branch\n", 0o644)
	})
	g.pushMR("feature", mrIID, s.feature)
	s.head = s.feature

	// Fake GitLab and OIDC provider on the host, reachable from the container.
	listenAddr := "127.0.0.1:0" // Docker Desktop forwards host.docker.internal to the host's loopback
	if runtime.GOOS == "linux" {
		listenAddr = "0.0.0.0:0" // reached through the host-gateway address
	}
	il, err := net.Listen("tcp", listenAddr)
	if err != nil {
		t.Fatal(err)
	}
	s.idp = oidctest.NewWithListener(il)
	t.Cleanup(s.idp.Close)
	// authentik-style issuer with a path and a trailing slash.
	s.issuer = fmt.Sprintf("http://%s:%d/application/o/ansibled/", hostAlias, il.Addr().(*net.TCPAddr).Port)
	s.idp.SetIssuer(s.issuer)

	l, err := net.Listen("tcp", listenAddr)
	if err != nil {
		t.Fatal(err)
	}
	s.fake = gitlabtest.NewWithListener(l)
	t.Cleanup(s.fake.Close)
	s.forgeURL = fmt.Sprintf("http://%s:%d", hostAlias, l.Addr().(*net.TCPAddr).Port)
	s.fake.SetBranch("main", s.main)
	s.fake.SetBranch("feature", s.feature)
	s.fake.AddMR(forge.MR{IID: mrIID, SourceBranch: "feature", HeadSHA: s.feature})
	s.fake.AddGroupMember(trustedGroup, alice)
	s.fake.AddPushEvent("main", s.main, alice)
	s.fake.AddPushEvent("feature", s.feature, alice)

	// Config directory mounted at /etc/ansibled.
	hookPort, webPort := freePort(t), freePort(t)
	s.webURL = fmt.Sprintf("http://127.0.0.1:%d", webPort)
	s.webhookURL = fmt.Sprintf("http://127.0.0.1:%d/webhook/gitlab", hookPort)
	s.cfgDir = filepath.Join(base, "etc")
	sec := filepath.Join(s.cfgDir, "secrets")
	for name, v := range map[string]string{
		"git_token": gitlabtest.Token, "webhook_secret": webhookSecret,
		"oidc_client_id": oidctest.ClientID, "oidc_client_secret": oidctest.ClientSecret,
		"metrics_token": metricsToken, "session_key": sessionKey,
	} {
		writeFile(t, filepath.Join(sec, name), v+"\n", 0o600)
	}
	writeFile(t, filepath.Join(sec, "ssh_key"), string(priv), 0o600)
	writeFile(t, filepath.Join(s.cfgDir, "ansibled.yml"), s.configYAML(), 0o644)

	docker(t, "volume", "create", dataVolume)
	s.runArgs = []string{
		"-v", s.cfgDir + ":" + containerCfg + ":ro",
		"-v", g.bare + ":" + containerGit + ":ro",
	}
	if runtime.GOOS == "linux" {
		s.runArgs = append(s.runArgs, "--add-host", hostAlias+":host-gateway")
	}

	// check-config, offline and online.
	for _, args := range [][]string{{"check-config"}, {"check-config", "-online"}} {
		out, err := dockerOut(slices.Concat([]string{"run", "--rm"}, s.runArgs, []string{daemonImage}, args)...)
		if err != nil {
			t.Fatalf("ansibled %v: %v\n%s", args, err, out)
		}
		if len(args) == 2 && !strings.Contains(out, "OIDC provider discovery: ok") {
			t.Fatalf("ansibled %v did not check OIDC discovery:\n%s", args, out)
		}
		t.Logf("ansibled %v: %s", args, out)
	}

	docker(t, slices.Concat(
		[]string{"run", "-d", "--name", daemonName, "--network", networkName,
			"-p", fmt.Sprintf("127.0.0.1:%d:8470", hookPort), "-p", fmt.Sprintf("127.0.0.1:%d:8471", webPort),
			"-v", dataVolume + ":/var/lib/ansibled"},
		s.runArgs, []string{daemonImage, "serve"})...)
	t.Cleanup(s.diagnostics) // runs before the container is removed
	s.waitHealthy()
}

func (s *suite) configYAML() string {
	return fmt.Sprintf(`site: %s
public_url: %s
listen:
  webhook: 0.0.0.0:8470
  web: 0.0.0.0:8471
git:
  type: gitlab
  url: %s
  project: %s
  token_file: /etc/ansibled/secrets/git_token
  webhook_secret_file: /etc/ansibled/secrets/webhook_secret
  trusted_group: %s
  clone_url: %s
repo:
  workdir: .
  setup: uv sync --frozen --no-dev
  venv_key_files: [pyproject.toml, uv.lock]
  collections_requirements: requirements.yml
ansible:
  inventory: inventory.yml
  playbooks: [site.yml]
  remote_user: root
  ssh_key_file: /etc/ansibled/secrets/ssh_key
  timeout: 15m
  env:
    SAMPLE_TARGET_HOST: %s
    SAMPLE_TARGET_PORT: "22"
    ANSIBLE_HOST_KEY_CHECKING: "False"
data_dir: /var/lib/ansibled
retention: 720h
web:
  metrics_token_file: /etc/ansibled/secrets/metrics_token
  session_key_file: /etc/ansibled/secrets/session_key
  oidc:
    issuer: %s
    client_id_file: /etc/ansibled/secrets/oidc_client_id
    client_secret_file: /etc/ansibled/secrets/oidc_client_secret
    allowed_groups: [sysadmins]
`, site, s.webURL, s.forgeURL, gitlabtest.DefaultProjectPath, trustedGroup, containerGit, targetName, s.issuer)
}

func (s *suite) waitHealthy() {
	t := s.t
	c := newClient(false)
	waitFor(t, 60*time.Second, "ansibled /healthz on both listeners", func() (bool, string) {
		for _, u := range []string{s.webURL + "/healthz", strings.TrimSuffix(s.webhookURL, "/webhook/gitlab") + "/healthz"} {
			resp, err := c.Get(u)
			if err != nil {
				return false, err.Error()
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return false, fmt.Sprintf("%s: %d", u, resp.StatusCode)
			}
		}
		return true, ""
	})
}

// diagnostics dumps the daemon's logs and the last run's log if the test failed.
func (s *suite) diagnostics() {
	t := s.t
	if !t.Failed() {
		return
	}
	out, _ := dockerOut("logs", "--tail", "300", daemonName)
	t.Logf("docker logs %s:\n%s", daemonName, out)
	id := s.lastRunID
	for _, st := range s.fake.Statuses() {
		id = max(id, runID(st))
	}
	if id > 0 {
		out, _ = dockerOut("run", "--rm", "-v", dataVolume+":/d:ro", "--entrypoint", "cat", daemonImage, fmt.Sprintf("/d/runs/%d/log.txt", id))
		t.Logf("log.txt of run %d:\n%s", id, out)
	}
	t.Logf("fake GitLab statuses:")
	for _, st := range s.fake.Statuses() {
		t.Logf("  %s %s %-8s %q %s", st.SHA[:8], st.Ref, st.State, st.Description, st.TargetURL)
	}
	for _, n := range s.fake.Notes(mrIID) {
		t.Logf("MR note %d (edits %d):\n%s", n.ID, n.Edits, n.Body)
	}
}

// ---------------------------------------------------------------------------
// helpers used by the scenarios

// waitRun waits until sha has a run whose statuses (after the first `skip`
// statuses for sha) reach a terminal state, and returns that run's statuses.
func (s *suite) waitRun(t *testing.T, sha string, skip int, timeout time.Duration) []gitlabtest.RecordedStatus {
	t.Helper()
	var got []gitlabtest.RecordedStatus
	waitFor(t, timeout, "run of "+sha[:8]+" to finish", func() (bool, string) {
		all := statusesFor(s.fake, sha)
		if len(all) <= skip {
			return false, fmt.Sprintf("statuses %v", statesOf(all))
		}
		got = all[skip:]
		return terminal(got[len(got)-1].State), fmt.Sprintf("statuses %v", statesOf(all))
	})
	if id := runID(got[0]); id > s.lastRunID {
		s.lastRunID = id
	}
	return got
}

// checkLifecycle asserts statuses are pending -> running -> want for one run.
func checkLifecycle(t *testing.T, ss []gitlabtest.RecordedStatus, ref string, want forge.StatusState, webURL string) int64 {
	t.Helper()
	wantStates := []string{"pending", "running", string(want)}
	if got := statesOf(ss); !slices.Equal(got, wantStates) {
		t.Fatalf("statuses %v, want %v (last description %q)", got, wantStates, ss[len(ss)-1].Description)
	}
	id := runID(ss[0])
	for _, st := range ss {
		if st.Ref != ref {
			t.Errorf("status ref %q, want %q", st.Ref, ref)
		}
		if runID(st) != id || id == 0 || !strings.HasPrefix(st.TargetURL, webURL+"/runs/") {
			t.Errorf("status target_url %q (run %d)", st.TargetURL, id)
		}
	}
	return id
}

// waitNote waits until the bot's single marker note satisfies cond.
func (s *suite) waitNote(t *testing.T, what string, cond func(body string) bool) gitlabtest.RecordedNote {
	t.Helper()
	var n gitlabtest.RecordedNote
	waitFor(t, 30*time.Second, "MR comment "+what, func() (bool, string) {
		ns := botNotes(s.fake, mrIID)
		if len(ns) != 1 {
			return false, fmt.Sprintf("%d marker notes", len(ns))
		}
		n = ns[0]
		return cond(n.Body), n.Body
	})
	if all := s.fake.Notes(mrIID); len(all) != 1 {
		t.Errorf("MR has %d notes, want exactly 1", len(all))
	}
	return n
}

// pushMRHead commits edit on feature, makes it the MR head everywhere and
// sends alice's push hook.
func (s *suite) pushMRHead(t *testing.T, msg string, edit func(dir string)) string {
	t.Helper()
	sha := s.repo.commit(msg, edit)
	s.repo.pushMR("feature", mrIID, sha)
	s.fake.SetBranch("feature", sha)
	s.fake.AddMR(forge.MR{IID: mrIID, SourceBranch: "feature", HeadSHA: sha})
	s.fake.AddPushEvent("feature", sha, alice)
	if code := sendHook(t, s.webhookURL, s.fake.PushHook(webhookSecret, "feature", sha, alice)); code != http.StatusAccepted {
		t.Fatalf("push hook: %d", code)
	}
	s.head = sha
	return sha
}

func (s *suite) getMetrics(t *testing.T) string {
	t.Helper()
	r := get(t, newClient(false), s.webURL+"/metrics", "Authorization", "Bearer "+metricsToken)
	if r.Status != http.StatusOK {
		t.Fatalf("/metrics with token: %d %s", r.Status, r.Body)
	}
	return r.Body
}

// ---------------------------------------------------------------------------
// scenarios

// a: without any webhook, main (M) and MR !1 (F) are queued and pass.
func (s *suite) startupCatchUp(t *testing.T) {
	ms := s.waitRun(t, s.main, 0, 10*time.Minute)
	s.mainRunID = checkLifecycle(t, ms, "main", forge.StatusSuccess, s.webURL)
	if d := ms[2].Description; !strings.Contains(d, "would change") {
		t.Errorf("main description %q", d)
	}
	fs := s.waitRun(t, s.feature, 0, 5*time.Minute)
	mrRun := checkLifecycle(t, fs, "feature", forge.StatusSuccess, s.webURL)
	if mrRun <= s.mainRunID {
		t.Errorf("MR run %d not after main run %d (FIFO)", mrRun, s.mainRunID)
	}

	n := s.waitNote(t, "to show the passed run", func(b string) bool { return strings.Contains(b, "✅") })
	s.noteID = n.ID
	for _, want := range []string{
		"<!-- ansibled:site=" + site + " -->", "dry run passed", "`" + s.feature[:8] + "`",
		"| target |", "Render sample config", fmt.Sprintf("%s/runs/%d", s.webURL, mrRun),
	} {
		if !strings.Contains(n.Body, want) {
			t.Errorf("MR comment lacks %q:\n%s", want, n.Body)
		}
	}
	// The main run never comments: the only note writes are for MR !1.
	posts := 0
	for _, r := range s.fake.Requests() {
		if strings.Contains(r, "/notes") {
			if !strings.Contains(r, fmt.Sprintf("/merge_requests/%d/notes", mrIID)) {
				t.Errorf("note request outside MR !%d: %s", mrIID, r)
			}
			if strings.HasPrefix(r, "POST ") {
				posts++
			}
		}
	}
	if posts != 1 {
		t.Errorf("%d notes created, want 1", posts)
	}
}

// b: wrong webhook token -> 401, GET -> 405/404.
func (s *suite) webhookAuth(t *testing.T) {
	before := len(s.fake.Statuses())
	if code := sendHook(t, s.webhookURL, s.fake.PushHook("wrong-secret", "feature", s.head, alice)); code != http.StatusUnauthorized {
		t.Errorf("wrong token: %d, want 401", code)
	}
	req := s.fake.PushHook("", "feature", s.head, alice) // no X-Gitlab-Token at all
	if code := sendHook(t, s.webhookURL, req); code != http.StatusUnauthorized {
		t.Errorf("missing token: %d, want 401", code)
	}
	r := get(t, newClient(false), s.webhookURL)
	if r.Status != http.StatusMethodNotAllowed && r.Status != http.StatusNotFound {
		t.Errorf("GET webhook: %d, want 405 or 404", r.Status)
	}
	// The web listener does not serve the webhook.
	req = s.fake.PushHook(webhookSecret, "feature", s.head, alice)
	if code := sendHook(t, s.webURL+"/webhook/gitlab", req); code == http.StatusAccepted {
		t.Errorf("web listener accepted a webhook")
	}
	time.Sleep(2 * time.Second)
	if after := len(s.fake.Statuses()); after != before {
		t.Errorf("rejected webhooks produced %d statuses", after-before)
	}
}

// c: alice pushes F2 -> MR run passes, the same comment is edited.
func (s *suite) trustedPush(t *testing.T) {
	f2 := s.pushMRHead(t, "feature: more notes", func(dir string) {
		writeFile(t, filepath.Join(dir, "NOTES.md"), "feature branch, take 2\n", 0o644)
	})
	ss := s.waitRun(t, f2, 0, 5*time.Minute)
	id := checkLifecycle(t, ss, "feature", forge.StatusSuccess, s.webURL)
	n := s.waitNote(t, "to show F2 passed", func(b string) bool {
		return strings.Contains(b, "✅") && strings.Contains(b, "`"+f2[:8]+"`")
	})
	if n.ID != s.noteID {
		t.Errorf("comment %d replaced by %d, want it edited", s.noteID, n.ID)
	}
	if n.Edits < 4 { // queued+running+finished of run 2, running+finished of run 1
		t.Errorf("comment edited %d times", n.Edits)
	}
	if !strings.Contains(n.Body, fmt.Sprintf("/runs/%d", id)) {
		t.Errorf("comment does not link run %d:\n%s", id, n.Body)
	}
}

// d: bob (not in the trusted group) pushes -> nothing runs.
func (s *suite) untrustedPush(t *testing.T) {
	before := len(s.fake.Statuses())
	rejectedBefore, _ := metricValue(s.getMetrics(t), "ansibled_webhooks_total", `accepted="false"`, `event="push"`)
	bobSHA := strings.Repeat("b0b", 13) + "b"
	for _, branch := range []string{"feature", "main"} {
		if code := sendHook(t, s.webhookURL, s.fake.PushHook(webhookSecret, branch, bobSHA, bob)); code != http.StatusAccepted {
			t.Fatalf("push hook: %d", code)
		}
	}
	waitFor(t, 20*time.Second, "untrusted pushes to be counted", func() (bool, string) {
		v, _ := metricValue(s.getMetrics(t), "ansibled_webhooks_total", `accepted="false"`, `event="push"`)
		return v >= rejectedBefore+2, fmt.Sprint(v)
	})
	time.Sleep(3 * time.Second)
	if after := s.fake.Statuses(); len(after) != before {
		t.Errorf("untrusted push produced statuses: %v", statesOf(after[before:]))
	}
}

// e: "/ansibled check" by alice re-runs the current head although it already ran.
func (s *suite) checkComment(t *testing.T) {
	prev := len(statusesFor(s.fake, s.head))
	// bob's comment is ignored.
	if code := sendHook(t, s.webhookURL, s.fake.NoteHook(webhookSecret, mrIID, bob, "/ansibled check")); code != http.StatusAccepted {
		t.Fatalf("note hook: %d", code)
	}
	if code := sendHook(t, s.webhookURL, s.fake.NoteHook(webhookSecret, mrIID, alice, "  /ansibled check please\n")); code != http.StatusAccepted {
		t.Fatalf("note hook: %d", code)
	}
	ss := s.waitRun(t, s.head, prev, 5*time.Minute)
	checkLifecycle(t, ss, "feature", forge.StatusSuccess, s.webURL)
	time.Sleep(2 * time.Second)
	if n := len(statusesFor(s.fake, s.head)); n != prev+3 {
		t.Errorf("%d new statuses, want exactly one more run (3)", n-prev)
	}
}

// f: a head with fail_me=true -> failed status, comment names the task only.
func (s *suite) failure(t *testing.T) {
	sha := s.pushMRHead(t, "feature: fail on purpose", func(dir string) {
		replaceInFile(t, filepath.Join(dir, "site.yml"), "fail_me: false", "fail_me: true")
	})
	ss := s.waitRun(t, sha, 0, 5*time.Minute)
	checkLifecycle(t, ss, "feature", forge.StatusFailed, s.webURL)
	if d := ss[2].Description; d != "1 failed on target" {
		t.Errorf("failed description %q", d)
	}
	n := s.waitNote(t, "to show the failure", func(b string) bool {
		return strings.Contains(b, "❌") && strings.Contains(b, "`"+sha[:8]+"`")
	})
	for _, want := range []string{"dry run failed", "Failed (1)", "target: Fail on request", "| target | 1 | 1 |"} {
		if !strings.Contains(n.Body, want) {
			t.Errorf("comment lacks %q:\n%s", want, n.Body)
		}
	}
	// No task messages (the fail msg, the tolerated failure's output).
	for _, bad := range []string{"fail_me is set", "non-zero return code", "/bin/false"} {
		if strings.Contains(n.Body, bad) {
			t.Errorf("comment contains task message %q:\n%s", bad, n.Body)
		}
	}
	if n.ID != s.noteID {
		t.Errorf("comment %d replaced by %d", s.noteID, n.ID)
	}
}

// g: /metrics needs the bearer token and has the drift of the main run.
func (s *suite) metrics(t *testing.T) {
	c := newClient(false)
	if r := get(t, c, s.webURL+"/metrics"); r.Status != http.StatusUnauthorized {
		t.Errorf("/metrics without token: %d", r.Status)
	}
	if r := get(t, c, s.webURL+"/metrics", "Authorization", "Bearer wrong"); r.Status != http.StatusUnauthorized {
		t.Errorf("/metrics with wrong token: %d", r.Status)
	}
	if r := get(t, c, strings.TrimSuffix(s.webhookURL, "/webhook/gitlab")+"/metrics", "Authorization", "Bearer "+metricsToken); r.Status == http.StatusOK {
		t.Errorf("webhook listener serves /metrics")
	}
	body := s.getMetrics(t)
	if !strings.Contains(body, "ansibled_runs_total") {
		t.Fatalf("no ansibled_runs_total:\n%s", body)
	}
	checks := []struct {
		name   string
		labels []string
		ok     func(float64) bool
	}{
		{"ansibled_drift_changed_tasks", []string{`host="target"`, `playbook="site.yml"`}, func(v float64) bool { return v >= 1 }},
		{"ansibled_drift_failed_tasks", []string{`host="target"`, `playbook="site.yml"`}, func(v float64) bool { return v == 0 }},
		{"ansibled_runs_total", []string{`kind="main"`, `state="success"`}, func(v float64) bool { return v == 1 }},
		{"ansibled_runs_total", []string{`kind="mr"`, `state="success"`}, func(v float64) bool { return v == 3 }},
		{"ansibled_runs_total", []string{`kind="mr"`, `state="failed"`}, func(v float64) bool { return v == 1 }},
		{"ansibled_webhooks_total", []string{`event="unauthorized"`, `accepted="false"`}, func(v float64) bool { return v >= 2 }},
		{"ansibled_queue_length", nil, func(v float64) bool { return v == 0 }},
	}
	for _, c := range checks {
		v, found := metricValue(body, c.name, c.labels...)
		if !found || !c.ok(v) {
			t.Errorf("%s%v = %v (found %v)", c.name, c.labels, v, found)
		}
	}
}

// h: the log pages need OIDC sign-in; only members of an allowed group get in.
func (s *suite) webUI(t *testing.T) {
	r := get(t, newClient(false), s.webURL+"/runs")
	if r.Status != http.StatusFound || !strings.HasPrefix(r.Header.Get("Location"), "/login") {
		t.Fatalf("/runs without session: %d Location %q", r.Status, r.Header.Get("Location"))
	}

	browser := newClient(true)
	s.idp.AddUser("code-alice", map[string]any{
		"sub": "alice-subject-0001", "preferred_username": "alice", "email": "alice@example.org",
		"groups": []any{"sysadmins"},
	})
	r = get(t, browser, s.webURL+"/login")
	if r.Status != http.StatusOK || !strings.HasSuffix(r.URL, "/runs") {
		t.Fatalf("sign-in as alice ended at %s with %d:\n%s", r.URL, r.Status, r.Body)
	}
	if !slices.ContainsFunc(browser.Jar.Cookies(mustURL(t, s.webURL)), func(c *http.Cookie) bool { return c.Name == "ansibled_session_"+site }) {
		t.Errorf("no session cookie after sign-in")
	}
	s.alice = browser
	// The daemon (in the container) exchanged the code and read userinfo.
	if n, m := s.idp.CountRequests(oidctest.Token), s.idp.CountRequests(oidctest.Userinfo); n != 1 || m != 1 {
		t.Errorf("provider saw %d token and %d userinfo requests, want 1 each", n, m)
	}
	for _, id := range []int64{s.mainRunID, s.lastRunID} {
		if !strings.Contains(r.Body, fmt.Sprintf(`href="/runs/%d"`, id)) {
			t.Errorf("/runs does not list run %d", id)
		}
	}
	for _, want := range []string{"alice", "startup", "comment", "push", "failed", "success"} {
		if !strings.Contains(r.Body, want) {
			t.Errorf("/runs lacks %q", want)
		}
	}

	r = get(t, browser, fmt.Sprintf("%s/runs/%d", s.webURL, s.mainRunID))
	if r.Status != http.StatusOK {
		t.Fatalf("/runs/%d: %d", s.mainRunID, r.Status)
	}
	for _, want := range []string{"TASK [Render sample config]", "PLAY RECAP", s.main, "would change"} {
		if !strings.Contains(r.Body, want) {
			t.Errorf("/runs/%d lacks %q", s.mainRunID, want)
		}
	}
	r = get(t, browser, fmt.Sprintf("%s/runs/%d/log.txt", s.webURL, s.mainRunID))
	if r.Status != http.StatusOK || !strings.HasPrefix(r.Header.Get("Content-Type"), "text/plain") {
		t.Errorf("log.txt: %d %q", r.Status, r.Header.Get("Content-Type"))
	}
	if !strings.Contains(r.Body, "ansible-playbook") || !strings.Contains(r.Body, "ok=") {
		t.Errorf("log.txt lacks ansible output:\n%s", r.Body)
	}
	if r := get(t, browser, s.webURL+"/runs/999999"); r.Status != http.StatusNotFound {
		t.Errorf("/runs/999999: %d", r.Status)
	}

	// bob signs in at the provider but is not in an allowed group.
	bobBrowser := newClient(true)
	s.idp.AddUser("code-bob", map[string]any{
		"sub": "bob-subject-0002", "preferred_username": "bob", "groups": []any{"users"},
	})
	r = get(t, bobBrowser, s.webURL+"/login")
	if r.Status != http.StatusForbidden || !strings.Contains(r.Body, "bob is not in an allowed group (sysadmins).") {
		t.Errorf("bob: %d at %s:\n%s", r.Status, r.URL, r.Body)
	}
	if r := get(t, newClientWithJar(bobBrowser), s.webURL+"/runs"); r.Status != http.StatusFound {
		t.Errorf("bob's /runs after refused sign-in: %d", r.Status)
	}
}

// i: a restart re-runs nothing and restores the drift gauges.
func (s *suite) restart(t *testing.T) {
	before := len(s.fake.Statuses())
	since := time.Now().Add(-time.Second).UTC().Format(time.RFC3339)
	docker(t, "restart", daemonName)
	s.waitHealthy()
	waitFor(t, 60*time.Second, "startup catch-up after restart", func() (bool, string) {
		out, _ := dockerOut("logs", "--since", since, daemonName)
		return strings.Contains(out, "startup catch-up done"), ""
	})
	time.Sleep(20 * time.Second)
	if after := s.fake.Statuses(); len(after) != before {
		t.Errorf("restart produced statuses: %v", statesOf(after[before:]))
	}
	body := s.getMetrics(t)
	if v, ok := metricValue(body, "ansibled_drift_changed_tasks", `host="target"`, `playbook="site.yml"`); !ok || v < 1 {
		t.Errorf("drift gauge after restart: %v (found %v)", v, ok)
	}
	// The session survives (session_key_file).
	if r := get(t, newClientWithJar(s.alice), s.webURL+"/runs"); r.Status != http.StatusOK {
		t.Errorf("/runs with alice's session after restart: %d", r.Status)
	}
}

// j: a slow MR run is canceled when the MR is merged.
func (s *suite) cancelOnMerge(t *testing.T) {
	sha := s.pushMRHead(t, "feature: slow", func(dir string) {
		p := filepath.Join(dir, "site.yml")
		replaceInFile(t, p, "fail_me: true", "fail_me: false")
		replaceInFile(t, p, "sleep_seconds: 0", "sleep_seconds: 60")
	})
	var id int64
	waitFor(t, 2*time.Minute, "slow run to start", func() (bool, string) {
		ss := statusesFor(s.fake, sha)
		if len(ss) > 0 {
			id = runID(ss[0])
		}
		return len(ss) >= 2 && ss[1].State == forge.StatusRunning, fmt.Sprint(statesOf(ss))
	})
	s.lastRunID = id
	logURL := fmt.Sprintf("%s/runs/%d/log.txt", s.webURL, id)
	waitFor(t, 2*time.Minute, "the slow task to start", func() (bool, string) {
		r := get(t, s.alice, logURL)
		return strings.Contains(r.Body, "TASK [Slow remote command]"), r.Body
	})
	time.Sleep(2 * time.Second) // let ssh get going

	s.fake.SetMRState(mrIID, "merged")
	t0 := time.Now()
	mr := forge.MR{IID: mrIID, SourceBranch: "feature", HeadSHA: sha, State: "merged"}
	if code := sendHook(t, s.webhookURL, s.fake.MRHook(webhookSecret, mr, forge.MRMerge)); code != http.StatusAccepted {
		t.Fatalf("merge hook: %d", code)
	}
	ss := s.waitRun(t, sha, 0, 30*time.Second)
	t.Logf("canceled %s after the merge hook", time.Since(t0).Round(100*time.Millisecond))
	checkLifecycle(t, ss, "feature", forge.StatusCanceled, s.webURL)
	if d := ss[2].Description; d != "MR merged" {
		t.Errorf("canceled description %q", d)
	}
	n := s.waitNote(t, "to show the cancel", func(b string) bool { return strings.Contains(b, "⏹") })
	if !strings.Contains(n.Body, "MR merged") || !strings.Contains(n.Body, sha[:8]) {
		t.Errorf("comment:\n%s", n.Body)
	}
	r := get(t, s.alice, fmt.Sprintf("%s/runs/%d", s.webURL, id))
	if !strings.Contains(r.Body, "state-canceled") || !strings.Contains(r.Body, "MR merged") {
		t.Errorf("run page does not show the cancel:\n%s", r.Body)
	}
	if r := get(t, s.alice, logURL); !strings.Contains(r.Body, "run canceled") {
		t.Errorf("log lacks the cancel:\n%s", r.Body)
	}
}
