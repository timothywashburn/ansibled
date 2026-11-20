// Command ansibled watches a git repository and dry-runs its Ansible playbooks.
// See DESIGN.md for behaviour.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/timothywashburn/ansibled/internal/config"
	"github.com/timothywashburn/ansibled/internal/daemon"
	"github.com/timothywashburn/ansibled/internal/engine"
	"github.com/timothywashburn/ansibled/internal/forge"
	"github.com/timothywashburn/ansibled/internal/forge/gitlab"
	"github.com/timothywashburn/ansibled/internal/metrics"
	"github.com/timothywashburn/ansibled/internal/model"
	"github.com/timothywashburn/ansibled/internal/oidc"
	"github.com/timothywashburn/ansibled/internal/queue"
	"github.com/timothywashburn/ansibled/internal/report"
	"github.com/timothywashburn/ansibled/internal/store"
	"github.com/timothywashburn/ansibled/internal/web"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const defaultConfig = "/etc/ansibled/ansibled.yml"

func usage() {
	fmt.Fprintf(os.Stderr, `ansibled %s

usage:
  ansibled serve        [-config path]
  ansibled check-config [-config path] [-online]
  ansibled run-once     [-config path] (-branch name [-sha sha] | -mr iid)
  ansibled version
`, version)
}

func main() {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		cmd, args = args[0], args[1:]
	}

	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	fs.Usage = usage
	configPath := fs.String("config", defaultConfig, "path to ansibled.yml")
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel()}))
	slog.SetDefault(log)

	var err error
	switch cmd {
	case "version":
		fmt.Println(version)
		return
	case "serve":
		_ = fs.Parse(args)
		err = serve(*configPath, log)
	case "check-config":
		online := fs.Bool("online", false, "also check the git token, project and trusted group, and OIDC discovery")
		_ = fs.Parse(args)
		err = checkConfig(*configPath, *online)
	case "run-once":
		branch := fs.String("branch", "", "branch to run (its head unless -sha is given)")
		sha := fs.String("sha", "", "commit to run (needs -branch so the right ref is fetched)")
		mr := fs.Int("mr", 0, "merge request whose head to run")
		_ = fs.Parse(args)
		err = runOnce(*configPath, *branch, *sha, *mr, log)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ansibled:", err)
		os.Exit(1)
	}
}

func logLevel() slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(os.Getenv("ANSIBLED_LOG_LEVEL"))); err != nil {
		return slog.LevelInfo
	}
	return l
}

// secrets read once at startup.
type secrets struct {
	forgeToken, webhookSecret, oidcID, oidcSecret, metricsToken string
	sessionKey                                                  []byte
}

func readSecrets(c *config.Config) (secrets, error) {
	var s secrets
	var errs []error
	read := func(p string, dst *string) {
		v, err := config.ReadSecret(p)
		if err != nil {
			errs = append(errs, err)
		}
		*dst = v
	}
	read(c.Git.TokenFile, &s.forgeToken)
	read(c.Git.WebhookSecretFile, &s.webhookSecret)
	read(c.Web.OIDC.ClientIDFile, &s.oidcID)
	read(c.Web.OIDC.ClientSecretFile, &s.oidcSecret)
	read(c.Web.MetricsTokenFile, &s.metricsToken)
	if c.Web.SessionKeyFile != "" {
		var k string
		read(c.Web.SessionKeyFile, &k)
		s.sessionKey = []byte(k)
	}
	// Read it, not just stat it: an existing key the daemon's user can't read
	// would only show up as every host being unreachable.
	if b, err := os.ReadFile(c.Ansible.SSHKeyFile); err != nil {
		errs = append(errs, fmt.Errorf("ansible.ssh_key_file: %w", err))
	} else if len(b) == 0 {
		errs = append(errs, fmt.Errorf("ansible.ssh_key_file: %s: empty", c.Ansible.SSHKeyFile))
	}
	return s, errors.Join(errs...)
}

func newForge(c *config.Config, s secrets, log *slog.Logger) (*gitlab.Client, error) {
	return gitlab.New(gitlab.Options{
		BaseURL:       c.Git.URL,
		Project:       c.Git.Project,
		Token:         s.forgeToken,
		WebhookSecret: s.webhookSecret,
		TrustedGroup:  c.Git.TrustedGroup,
		CloneURL:      c.Git.CloneURL,
		Logger:        log.With("component", "gitlab"),
	})
}

func newSignIn(c *config.Config, s secrets, log *slog.Logger) (*oidc.Provider, error) {
	return oidc.New(oidc.Options{
		Issuer:        c.Web.OIDC.Issuer,
		ClientID:      s.oidcID,
		ClientSecret:  s.oidcSecret,
		Scopes:        c.Web.OIDC.Scopes,
		GroupsClaim:   c.Web.OIDC.GroupsClaim,
		AllowedGroups: c.Web.OIDC.AllowedGroups,
		Logger:        log.With("component", "oidc"),
	})
}

func serve(path string, log *slog.Logger) error {
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	sec, err := readSecrets(cfg)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	f, err := newForge(cfg, sec, log)
	if err != nil {
		return err
	}
	signIn, err := newSignIn(cfg, sec, log)
	if err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, "ansibled.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	eng, err := engine.New(engine.Options{Config: cfg, Source: f, Logger: log.With("component", "engine")})
	if err != nil {
		return err
	}
	m := metrics.New()

	d := daemon.New(cfg, f, st, m, eng, log.With("component", "daemon"))
	q := queue.New(queue.Options{
		Store:         st,
		Runner:        eng,
		Hooks:         d,
		Describe:      report.Describe,
		Logger:        log.With("component", "queue"),
		OnQueueLength: m.SetQueueLength,
	})
	d.Queue = q

	srv, err := web.New(web.Options{
		Config:       cfg,
		Forge:        f,
		SignIn:       signIn,
		Runs:         st,
		LogPath:      eng.LogPath,
		Metrics:      m,
		MetricsToken: sec.metricsToken,
		SessionKey:   sec.sessionKey,
		OnEvents:     d.HandleEvents,
		Logger:       log.With("component", "web"),
	})
	if err != nil {
		return err
	}

	log.Info("starting ansibled", "version", version, "site", cfg.Site, "webhook", cfg.Listen.Webhook, "web", cfg.Listen.Web)
	d.RestoreMetrics(ctx)
	if err := q.Start(ctx); err != nil {
		return err
	}
	if err := d.StartSchedule(ctx); err != nil {
		return err
	}
	go d.CatchUp(ctx)
	go d.Housekeep(ctx)

	webErr := srv.Run(ctx) // blocks until ctx is done (or a fatal listener error)
	stop()
	q.Wait()
	log.Info("ansibled stopped")
	if webErr != nil && !errors.Is(webErr, context.Canceled) {
		return webErr
	}
	return nil
}

func checkConfig(path string, online bool) error {
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	sec, err := readSecrets(cfg)
	if err != nil {
		return err
	}
	fmt.Println("config and secret files: ok")
	if !online {
		return nil
	}
	discard := slog.New(slog.DiscardHandler)
	f, err := newForge(cfg, sec, discard)
	if err != nil {
		return err
	}
	signIn, err := newSignIn(cfg, sec, discard)
	if err != nil {
		return err
	}
	var failed []error
	if errs := f.Check(context.Background()); len(errs) > 0 {
		failed = append(failed, fmt.Errorf("git check failed:\n  %w", errors.Join(errs...)))
	} else {
		fmt.Println("git token, project and trusted group: ok")
	}
	if err := signIn.Check(context.Background()); err != nil {
		failed = append(failed, fmt.Errorf("OIDC check failed:\n  %w", err))
	} else {
		fmt.Println("OIDC provider discovery: ok")
		if strings.HasPrefix(cfg.Web.OIDC.Issuer, "http://") {
			fmt.Println("warning: web.oidc.issuer uses plain http; use https outside tests and labs")
		}
	}
	return errors.Join(failed...)
}

// runOnce runs the engine in the foreground and prints the summary. It writes
// nothing to the forge or the database; its files live in <data_dir>/runs/0.
func runOnce(path, branch, sha string, mr int, log *slog.Logger) error {
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	// The token is only optional when nothing needs the API or authenticated git.
	token, tokErr := config.ReadSecret(cfg.Git.TokenFile)
	if needAPI := mr != 0 || sha == "" || cfg.Git.CloneURL == ""; needAPI && tokErr != nil {
		return tokErr
	}
	f, err := gitlab.New(gitlab.Options{
		BaseURL: cfg.Git.URL, Project: cfg.Git.Project, Token: token,
		// run-once never receives webhooks or signs anyone in.
		WebhookSecret: "unused-by-run-once",
		TrustedGroup:  cfg.Git.TrustedGroup, CloneURL: cfg.Git.CloneURL,
		Logger: log.With("component", "gitlab"),
	})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	r := &model.Run{ID: 0, Mode: model.ModeCheck, Trigger: model.TriggerManual, State: model.StateRunning}
	switch {
	case mr != 0:
		m, err := f.GetMR(ctx, mr)
		if err != nil {
			return err
		}
		r.Kind, r.MRIID, r.Ref, r.SHA = model.KindMR, mr, m.SourceBranch, m.HeadSHA
	case branch != "":
		r.Kind, r.Ref, r.SHA = model.KindMain, branch, sha
		if sha == "" {
			if r.SHA, err = f.BranchHead(ctx, branch); err != nil {
				return err
			}
		}
	default:
		return errors.New("run-once: give -branch (optionally with -sha) or -mr")
	}
	r.Key = model.Key(r.Kind, r.MRIID)

	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return err
	}
	eng, err := engine.New(engine.Options{Config: cfg, Source: f, Logger: log.With("component", "engine")})
	if err != nil {
		return err
	}
	_ = eng.DeleteRunFiles(0)
	fmt.Printf("running %s %s at %s; log: %s\n", r.Kind, r.Ref, r.ShortSHA(), eng.LogPath(0))
	state, sum := eng.Run(ctx, r)
	r.State, r.Summary = state, sum
	fmt.Printf("\nresult: %s (%s)\n", state, report.Describe(r))
	printSummary(sum)
	if state != model.StateSuccess {
		os.Exit(3)
	}
	return nil
}

func printSummary(s *model.Summary) {
	if s == nil {
		return
	}
	if s.FailedStep != "" {
		fmt.Printf("failed step: %s: %s\n", s.FailedStep, s.Error)
	}
	for _, p := range s.Playbooks {
		fmt.Printf("\n%s (exit %d)\n", p.Playbook, p.ExitCode)
		for _, h := range p.Hosts {
			u := ""
			if h.Unreachable {
				u = " UNREACHABLE"
			}
			fmt.Printf("  %-24s changed=%d failed=%d%s\n", h.Host, h.Changed, h.Failed, u)
			for _, t := range h.ChangedTasks {
				fmt.Printf("      ~ %s\n", t)
			}
			for _, t := range h.FailedTasks {
				fmt.Printf("      ! %s\n", t)
			}
		}
	}
}

var _ forge.Forge = (*gitlab.Client)(nil)
