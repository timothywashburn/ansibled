// Package config loads and validates ansibled.yml (DESIGN.md "Configuration").
package config

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"go.yaml.in/yaml/v3"
)

// Config is the whole ansibled.yml.
type Config struct {
	Site       string   `yaml:"site"`
	PublicURL  string   `yaml:"public_url"`
	Listen     Listen   `yaml:"listen"`
	Git        Git      `yaml:"git"`
	Repo       Repo     `yaml:"repo"`
	Ansible    Ansible  `yaml:"ansible"`
	OverlayDir string   `yaml:"overlay_dir"`
	DataDir    string   `yaml:"data_dir"`
	Schedule   Schedule `yaml:"schedule"`
	Retention  Duration `yaml:"retention"`
	Web        Web      `yaml:"web"`

	// Path the config was loaded from (not part of the file).
	Path string `yaml:"-"`
}

type Listen struct {
	Webhook string `yaml:"webhook"`
	Web     string `yaml:"web"`
}

type Git struct {
	Type              string `yaml:"type"`
	URL               string `yaml:"url"`
	Project           string `yaml:"project"`
	TokenFile         string `yaml:"token_file"`
	WebhookSecretFile string `yaml:"webhook_secret_file"`
	TrustedGroup      string `yaml:"trusted_group"`
	// CloneURL overrides the git fetch URL (tests, mirrors). Optional.
	CloneURL string `yaml:"clone_url"`
}

type Repo struct {
	Workdir                 string   `yaml:"workdir"`
	Setup                   string   `yaml:"setup"`
	VenvKeyFiles            []string `yaml:"venv_key_files"`
	CollectionsRequirements string   `yaml:"collections_requirements"`
}

type Ansible struct {
	Inventory  string            `yaml:"inventory"`
	Playbooks  []string          `yaml:"playbooks"`
	ExtraArgs  []string          `yaml:"extra_args"`
	RemoteUser string            `yaml:"remote_user"`
	SSHKeyFile string            `yaml:"ssh_key_file"`
	Timeout    Duration          `yaml:"timeout"`
	Env        map[string]string `yaml:"env"`
}

type Schedule struct {
	Nightly  string `yaml:"nightly"`
	Timezone string `yaml:"timezone"`
}

type Web struct {
	MetricsTokenFile string `yaml:"metrics_token_file"`
	SessionKeyFile   string `yaml:"session_key_file"`
	OIDC             OIDC   `yaml:"oidc"`
}

// OIDC is the OpenID Connect provider that signs people in to the web UI.
type OIDC struct {
	Issuer           string   `yaml:"issuer"`
	ClientIDFile     string   `yaml:"client_id_file"`
	ClientSecretFile string   `yaml:"client_secret_file"`
	AllowedGroups    []string `yaml:"allowed_groups"`
	GroupsClaim      string   `yaml:"groups_claim"`
	Scopes           []string `yaml:"scopes"`
}

// Defaults of web.oidc.
const DefaultGroupsClaim = "groups"

// DefaultScopes returns the default web.oidc.scopes.
func DefaultScopes() []string { return []string{"openid", "profile", "email"} }

// Duration is a time.Duration that unmarshals from strings like "45m" or "720h".
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	d.Duration = v
	return nil
}

func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

var siteRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// Load reads, defaults and validates a config file.
func Load(p string) (*Config, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	c, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	c.Path = p
	return c, nil
}

// Parse decodes (rejecting unknown keys), applies defaults and validates.
func Parse(b []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, err
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Git.Type == "" {
		c.Git.Type = "gitlab"
	}
	if c.Repo.Workdir == "" {
		c.Repo.Workdir = "."
	}
	if c.DataDir == "" {
		c.DataDir = "/var/lib/ansibled"
	}
	if c.Ansible.Timeout.Duration == 0 {
		c.Ansible.Timeout.Duration = 45 * time.Minute
	}
	if c.Retention.Duration == 0 {
		c.Retention.Duration = 30 * 24 * time.Hour
	}
	if c.Schedule.Timezone == "" {
		c.Schedule.Timezone = "UTC"
	}
	if c.Web.OIDC.GroupsClaim == "" {
		c.Web.OIDC.GroupsClaim = DefaultGroupsClaim
	}
	if len(c.Web.OIDC.Scopes) == 0 {
		c.Web.OIDC.Scopes = DefaultScopes()
	}
	c.PublicURL = strings.TrimRight(c.PublicURL, "/")
	c.Git.URL = strings.TrimRight(c.Git.URL, "/")
}

// Validate checks everything that can be checked without touching the network
// or the secret files.
func (c *Config) Validate() error {
	var errs []error
	add := func(f string, a ...any) { errs = append(errs, fmt.Errorf(f, a...)) }

	if !siteRe.MatchString(c.Site) {
		add("site: must match %s", siteRe)
	}
	if u, err := url.Parse(c.PublicURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		add("public_url: must be an absolute http(s) URL")
	}
	for name, addr := range map[string]string{"listen.webhook": c.Listen.Webhook, "listen.web": c.Listen.Web} {
		if _, _, err := net.SplitHostPort(addr); err != nil {
			add("%s: must be host:port: %v", name, err)
		}
	}

	switch c.Git.Type {
	case "gitlab":
	default:
		add("git.type: %q is not supported (gitlab)", c.Git.Type)
	}
	if u, err := url.Parse(c.Git.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		add("git.url: must be an absolute http(s) URL")
	}
	if c.Git.Project == "" {
		add("git.project: required")
	}
	if c.Git.TrustedGroup == "" {
		add("git.trusted_group: required")
	}
	req := map[string]string{
		"git.token_file":              c.Git.TokenFile,
		"git.webhook_secret_file":     c.Git.WebhookSecretFile,
		"ansible.ssh_key_file":        c.Ansible.SSHKeyFile,
		"web.metrics_token_file":      c.Web.MetricsTokenFile,
		"web.oidc.client_id_file":     c.Web.OIDC.ClientIDFile,
		"web.oidc.client_secret_file": c.Web.OIDC.ClientSecretFile,
	}
	for name, v := range req {
		if v == "" {
			add("%s: required", name)
		} else if !filepath.IsAbs(v) {
			add("%s: must be an absolute path", name)
		}
	}
	if c.Web.SessionKeyFile != "" && !filepath.IsAbs(c.Web.SessionKeyFile) {
		add("web.session_key_file: must be an absolute path")
	}
	o := c.Web.OIDC
	if u, err := url.Parse(o.Issuer); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.RawQuery != "" || u.Fragment != "" {
		add("web.oidc.issuer: required, an absolute http(s) URL without query or fragment")
	}
	if len(o.AllowedGroups) == 0 {
		add("web.oidc.allowed_groups: list at least one group")
	}
	for _, g := range o.AllowedGroups {
		if strings.TrimSpace(g) == "" {
			add("web.oidc.allowed_groups: empty group name")
		}
	}
	if strings.TrimSpace(o.GroupsClaim) == "" {
		add("web.oidc.groups_claim: must not be empty")
	}
	if !slices.Contains(o.Scopes, "openid") {
		add("web.oidc.scopes: must include openid")
	}
	for _, sc := range o.Scopes {
		if sc == "" || strings.ContainsAny(sc, " \t\r\n\"\\") {
			add("web.oidc.scopes: %q is not a valid scope", sc)
		}
	}

	if !relClean(c.Repo.Workdir) {
		add("repo.workdir: must be a relative path inside the repo")
	}
	if strings.TrimSpace(c.Repo.Setup) == "" {
		add("repo.setup: required (e.g. \"uv sync --frozen --no-dev\")")
	}
	if len(c.Repo.VenvKeyFiles) == 0 {
		add("repo.venv_key_files: list at least one file (e.g. uv.lock)")
	}
	for _, f := range c.Repo.VenvKeyFiles {
		if !relClean(f) {
			add("repo.venv_key_files: %q must be a relative path", f)
		}
	}
	if c.Repo.CollectionsRequirements != "" && !relClean(c.Repo.CollectionsRequirements) {
		add("repo.collections_requirements: must be a relative path")
	}

	if c.Ansible.Inventory == "" || !relClean(c.Ansible.Inventory) {
		add("ansible.inventory: required, relative to repo.workdir")
	}
	if len(c.Ansible.Playbooks) == 0 {
		add("ansible.playbooks: list at least one playbook")
	}
	for _, p := range c.Ansible.Playbooks {
		if !relClean(p) {
			add("ansible.playbooks: %q must be a relative path", p)
		}
	}
	for _, a := range c.Ansible.ExtraArgs {
		switch a {
		case "--check", "-C", "--diff", "-D":
			add("ansible.extra_args: %s is always set by ansibled", a)
		}
	}
	for k := range c.Ansible.Env {
		if strings.HasPrefix(k, "ANSIBLED_") {
			add("ansible.env: %s is reserved", k)
		}
	}

	if c.OverlayDir != "" && !filepath.IsAbs(c.OverlayDir) {
		add("overlay_dir: must be an absolute path")
	}
	if !filepath.IsAbs(c.DataDir) {
		add("data_dir: must be an absolute path")
	}
	if c.Schedule.Nightly != "" {
		if _, err := cron.ParseStandard(c.Schedule.Nightly); err != nil {
			add("schedule.nightly: %v", err)
		}
	}
	if _, err := time.LoadLocation(c.Schedule.Timezone); err != nil {
		add("schedule.timezone: %v", err)
	}
	if c.Ansible.Timeout.Duration < time.Minute {
		add("ansible.timeout: must be at least 1m")
	}
	if c.Retention.Duration < time.Hour {
		add("retention: must be at least 1h")
	}
	return errors.Join(errs...)
}

// relClean reports whether p is a non-empty relative path that stays inside its root.
func relClean(p string) bool {
	if p == "" || path.IsAbs(p) || filepath.IsAbs(p) {
		return false
	}
	c := path.Clean(filepath.ToSlash(p))
	return c != ".." && !strings.HasPrefix(c, "../")
}

// StatusName is the commit status name for this site.
func (c *Config) StatusName() string { return "ansibled/" + c.Site }

// CommentMarker is the hidden marker identifying this site's MR comment.
func (c *Config) CommentMarker() string { return "<!-- ansibled:site=" + c.Site + " -->" }

// RunURL is the log page link for a run.
func (c *Config) RunURL(id int64) string { return fmt.Sprintf("%s/runs/%d", c.PublicURL, id) }

// Location returns the schedule timezone.
func (c *Config) Location() *time.Location {
	l, err := time.LoadLocation(c.Schedule.Timezone)
	if err != nil {
		return time.UTC
	}
	return l
}

// ReadSecret reads a secret file and trims surrounding whitespace.
func ReadSecret(p string) (string, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", fmt.Errorf("%s: empty", p)
	}
	return s, nil
}
