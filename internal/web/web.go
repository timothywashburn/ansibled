// Package web serves ansibled's HTTP endpoints (DESIGN.md "Web UI and auth"):
// the forge webhook, the web UI behind OpenID Connect sign-in (overview, run
// list, run pages with a live log console, re-run and cancel), /metrics
// behind a bearer token, and /healthz.
package web

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/timothywashburn/ansibled/internal/config"
	"github.com/timothywashburn/ansibled/internal/forge"
	"github.com/timothywashburn/ansibled/internal/metrics"
	"github.com/timothywashburn/ansibled/internal/model"
	"github.com/timothywashburn/ansibled/internal/oidc"
)

// RunSource is implemented by the store.
type RunSource interface {
	// GetRun returns (nil, nil) if the run does not exist.
	GetRun(ctx context.Context, id int64) (*model.Run, error)
	// FilterRuns returns the runs matching f, newest first.
	FilterRuns(ctx context.Context, f model.RunFilter, limit, offset int) ([]*model.Run, error)
	// ListByState returns the runs in one of states, oldest first.
	ListByState(ctx context.Context, states ...model.State) ([]*model.Run, error)
	// LatestFinished returns the last run of one of kinds that finished
	// (success, failed or error), or (nil, nil).
	LatestFinished(ctx context.Context, kinds ...model.Kind) (*model.Run, error)
}

// Actions are the buttons of the web UI. *daemon.Daemon implements it.
// Errors of type daemon.ActionError are shown to the user as they are.
type Actions interface {
	// Rerun queues the run again and returns the new run.
	Rerun(ctx context.Context, id int64, user string) (*model.Run, error)
	// Cancel cancels a queued or running run.
	Cancel(ctx context.Context, id int64, user string) error
	// RunMain queues a main run of the default branch's head.
	RunMain(ctx context.Context, user string) (*model.Run, error)
}

// Links is optionally implemented by the Forge to link commits and MRs.
type Links interface {
	CommitURL(sha string) string // "" if unknown
	MRURL(iid int) string        // "" if unknown
}

// SignIn signs people in to the web UI. *oidc.Provider implements it.
type SignIn interface {
	// AuthCodeURL builds the provider's authorization URL; verifier is the
	// PKCE code verifier. It fails if the provider cannot be reached.
	AuthCodeURL(state, verifier, redirectURL string) (string, error)
	// Login exchanges the code and returns who signed in and whether they
	// are in an allowed group.
	Login(ctx context.Context, code, verifier, redirectURL string) (oidc.Identity, error)
}

// Options configure a Server.
type Options struct {
	Config  *config.Config
	Forge   forge.Forge // Name and HandleWebhook
	SignIn  SignIn      // web UI sign-in
	Runs    RunSource
	LogPath func(id int64) string // path of a run's log.txt
	// Details reads the changed/failed results of playbook n of a run (the
	// Changes tab). Optional.
	Details func(id int64, n int) ([]model.TaskDetail, error)
	// Actions enables the re-run and cancel buttons. Optional.
	Actions      Actions
	Metrics      *metrics.Metrics
	MetricsToken string
	SessionKey   []byte // HMAC key for cookies; if empty, 32 random bytes
	// OnEvents receives accepted webhook events. It is called in a new
	// goroutine with a background context.
	OnEvents func(ctx context.Context, events []forge.Event)
	Logger   *slog.Logger
}

// Server is ansibled's HTTP front end.
type Server struct {
	o      Options
	cfg    *config.Config
	log    *slog.Logger
	key    []byte
	pages  *pages
	assets *assets
	links  Links

	webhookPath  string
	secure       bool   // cookies get the Secure flag
	sessionName  string // cookie names include the site, so instances on one host don't collide
	oauthName    string
	redirectURL  string
	now          func() time.Time
	retry        time.Duration // bind retry interval
	warnEvery    time.Duration // at most one bind warning per interval
	shutdownWait time.Duration
	streamPoll   time.Duration // how often a log stream looks for new output
}

// Timings and limits.
const (
	sessionTTL      = 12 * time.Hour
	oauthTTL        = 10 * time.Minute
	maxWebhookBody  = 5 << 20
	logInlineLimit  = 2 << 20
	runsPerPage     = 50
	streamChunk     = 256 << 10
	defaultPoll     = 250 * time.Millisecond
	defaultRetry    = 5 * time.Second
	defaultWarn     = time.Minute
	defaultShutdown = 10 * time.Second
)

// New validates the options and builds a Server.
func New(o Options) (*Server, error) {
	if o.Config == nil {
		return nil, errors.New("web: Config is required")
	}
	if o.Forge == nil {
		return nil, errors.New("web: Forge is required")
	}
	if o.SignIn == nil {
		return nil, errors.New("web: SignIn is required")
	}
	if o.Runs == nil {
		return nil, errors.New("web: Runs is required")
	}
	if o.LogPath == nil {
		return nil, errors.New("web: LogPath is required")
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	key := o.SessionKey
	if len(key) == 0 {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, fmt.Errorf("web: session key: %w", err)
		}
	}
	a, err := loadAssets()
	if err != nil {
		return nil, err
	}
	p, err := loadPages(a)
	if err != nil {
		return nil, err
	}
	links, _ := o.Forge.(Links)
	cfg := o.Config
	return &Server{
		o:            o,
		cfg:          cfg,
		log:          o.Logger,
		key:          key,
		pages:        p,
		assets:       a,
		links:        links,
		streamPoll:   defaultPoll,
		webhookPath:  "/webhook/" + o.Forge.Name(),
		secure:       strings.HasPrefix(cfg.PublicURL, "https://"),
		sessionName:  "ansibled_session_" + cfg.Site,
		oauthName:    "ansibled_oauth_" + cfg.Site,
		redirectURL:  cfg.PublicURL + "/oauth/callback",
		now:          time.Now,
		retry:        defaultRetry,
		warnEvery:    defaultWarn,
		shutdownWait: defaultShutdown,
	}, nil
}

// Handler returns every route on one mux (webhook, UI, metrics, healthz).
func (s *Server) Handler() http.Handler { return s.mux(true, true) }

// mux builds a handler with the webhook routes, the web routes, or both.
func (s *Server) mux(webhook, web bool) http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /healthz", s.healthz)
	if webhook {
		m.HandleFunc("POST "+s.webhookPath, s.webhook)
	}
	if web {
		m.Handle("GET /{$}", s.requireSession(s.overview))
		m.HandleFunc("GET /static/{name}", s.static)
		m.HandleFunc("GET /metrics", s.metrics)
		m.HandleFunc("GET /login", s.login)
		m.HandleFunc("GET /oauth/callback", s.callback)
		m.HandleFunc("GET /logout", s.logout)
		m.HandleFunc("POST /logout", s.logout)
		m.Handle("GET /runs", s.requireSession(s.runList))
		m.Handle("GET /runs/{id}", s.requireSession(s.runPage))
		m.Handle("GET /runs/{id}/log.txt", s.requireSession(s.runLog))
		m.Handle("GET /runs/{id}/stream", s.requireSession(s.runStream))
		m.Handle("POST /runs/{id}/rerun", s.requireSession(s.rerun))
		m.Handle("POST /runs/{id}/cancel", s.requireSession(s.cancel))
		m.Handle("POST /run/main", s.requireSession(s.runMain))
	}
	return securityHeaders(m)
}

// securityHeaders sets headers every response gets.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; "+
			"connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}

// webhook verifies and parses a forge webhook and hands the events off.
func (s *Server) webhook(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxWebhookBody)
	events, err := s.o.Forge.HandleWebhook(r)
	switch {
	case errors.Is(err, forge.ErrUnauthorized):
		s.log.Warn("webhook rejected: bad secret", "remote", r.RemoteAddr)
		s.o.Metrics.Webhook("unauthorized", false)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	case err != nil:
		s.log.Warn("webhook rejected: invalid", "remote", r.RemoteAddr, "err", err)
		s.o.Metrics.Webhook("invalid", false)
		http.Error(w, "invalid webhook", http.StatusBadRequest)
		return
	}
	if s.o.OnEvents != nil && len(events) > 0 {
		go s.o.OnEvents(context.Background(), events)
	}
	w.WriteHeader(http.StatusAccepted)
}

// Run serves until ctx is done, then shuts down gracefully. listen.webhook
// serves the webhook and /healthz; listen.web serves everything else and
// /healthz; equal addresses share one listener. Each address is bound with
// retries until it becomes available.
func (s *Server) Run(ctx context.Context) error {
	type listener struct {
		name, addr string
		h          http.Handler
	}
	var ls []listener
	if s.cfg.Listen.Webhook == s.cfg.Listen.Web {
		ls = []listener{{"web+webhook", s.cfg.Listen.Web, s.mux(true, true)}}
	} else {
		ls = []listener{
			{"webhook", s.cfg.Listen.Webhook, s.mux(true, false)},
			{"web", s.cfg.Listen.Web, s.mux(false, true)},
		}
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	for _, l := range ls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.serve(ctx, l.name, l.addr, l.h); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
				cancel()
			}
		}()
	}
	wg.Wait()
	return firstErr
}

// serve binds addr (retrying) and serves h until ctx is done.
func (s *Server) serve(ctx context.Context, name, addr string, h http.Handler) error {
	ln, err := s.bind(ctx, name, addr)
	if err != nil {
		return nil // ctx done before the address became available
	}
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       time.Minute,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
		BaseContext:       func(net.Listener) context.Context { return context.WithoutCancel(ctx) },
	}
	s.log.Info("listening", "listener", name, "addr", ln.Addr().String())

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("%s listener %s: %w", name, addr, err)
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), s.shutdownWait)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		s.log.Warn("shutdown", "listener", name, "err", err)
		_ = srv.Close()
	}
	<-errc
	return nil
}

// bind listens on addr, retrying every s.retry until it works or ctx is done.
// The address may belong to an interface that comes up later (e.g. a VPN).
func (s *Server) bind(ctx context.Context, name, addr string) (net.Listener, error) {
	var lastWarn time.Time
	var lc net.ListenConfig
	for {
		ln, err := lc.Listen(ctx, "tcp", addr)
		if err == nil {
			return ln, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if now := time.Now(); lastWarn.IsZero() || now.Sub(lastWarn) >= s.warnEvery {
			lastWarn = now
			s.log.Warn("cannot bind, retrying", "listener", name, "addr", addr, "every", s.retry, "err", err)
		}
		t := time.NewTimer(s.retry)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}
