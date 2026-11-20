package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const valid = `
site: mysite
public_url: http://10.0.0.1:9251/
listen:
  webhook: 10.0.0.1:9250
  web: 10.0.0.1:9251
git:
  url: https://gitlab.example.org/
  project: myorg/infra
  token_file: /s/token
  webhook_secret_file: /s/hook
  trusted_group: myorg/sysadmins
repo:
  workdir: ansible
  setup: uv sync --frozen
  venv_key_files: [uv.lock]
ansible:
  inventory: inventories/prod
  playbooks: [site.yml]
  ssh_key_file: /s/key
schedule:
  nightly: "0 4 * * *"
  timezone: America/Los_Angeles
web:
  metrics_token_file: /s/metrics
  oidc:
    issuer: https://auth.example.org/application/o/ansibled/
    client_id_file: /s/oidc_id
    client_secret_file: /s/oidc_secret
    allowed_groups: [sysadmins]
`

func TestParseValidAndDefaults(t *testing.T) {
	c, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if c.Git.Type != "gitlab" || c.DataDir != "/var/lib/ansibled" {
		t.Errorf("defaults not applied: %+v", c)
	}
	if c.Ansible.Timeout.Duration != 45*time.Minute || c.Retention.Duration != 720*time.Hour {
		t.Errorf("duration defaults: %v %v", c.Ansible.Timeout, c.Retention)
	}
	if c.PublicURL != "http://10.0.0.1:9251" || c.Git.URL != "https://gitlab.example.org" {
		t.Errorf("trailing slashes not trimmed: %q %q", c.PublicURL, c.Git.URL)
	}
	if got := c.RunURL(7); got != "http://10.0.0.1:9251/runs/7" {
		t.Errorf("RunURL = %q", got)
	}
	if c.StatusName() != "ansibled/mysite" || c.CommentMarker() != "<!-- ansibled:site=mysite -->" {
		t.Errorf("names: %q %q", c.StatusName(), c.CommentMarker())
	}
	if c.Location().String() != "America/Los_Angeles" {
		t.Errorf("location %v", c.Location())
	}
	o := c.Web.OIDC
	if o.Issuer != "https://auth.example.org/application/o/ansibled/" {
		t.Errorf("issuer changed: %q", o.Issuer)
	}
	if o.GroupsClaim != "groups" || !slices.Equal(o.Scopes, []string{"openid", "profile", "email"}) {
		t.Errorf("oidc defaults: claim %q scopes %v", o.GroupsClaim, o.Scopes)
	}
	if !slices.Equal(o.AllowedGroups, []string{"sysadmins"}) || o.ClientIDFile != "/s/oidc_id" || o.ClientSecretFile != "/s/oidc_secret" {
		t.Errorf("oidc: %+v", o)
	}
}

func TestParseOIDCOverrides(t *testing.T) {
	src := strings.Replace(valid, "allowed_groups: [sysadmins]",
		"allowed_groups: [sysadmins, ops]\n    groups_claim: roles\n    scopes: [openid, groups]", 1)
	c, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	o := c.Web.OIDC
	if o.GroupsClaim != "roles" || !slices.Equal(o.Scopes, []string{"openid", "groups"}) || len(o.AllowedGroups) != 2 {
		t.Errorf("oidc: %+v", o)
	}
}

func TestParseRejects(t *testing.T) {
	cases := map[string]struct{ from, to, want string }{
		"unknown key":       {"site: mysite", "site: mysite\nbogus: 1", "bogus"},
		"bad site":          {"site: mysite", "site: 'my site'", "site:"},
		"escaping path":     {"workdir: ansible", "workdir: ../x", "repo.workdir"},
		"abs inventory":     {"inventory: inventories/prod", "inventory: /etc/x", "ansible.inventory"},
		"check in args":     {"playbooks: [site.yml]", "playbooks: [site.yml]\n  extra_args: [--check]", "always set"},
		"relative token":    {"token_file: /s/token", "token_file: s/token", "git.token_file"},
		"bad cron":          {`"0 4 * * *"`, `"61 4 * * *"`, "schedule.nightly"},
		"bad tz":            {"America/Los_Angeles", "Mars/Olympus", "schedule.timezone"},
		"bad duration":      {"ssh_key_file: /s/key", "ssh_key_file: /s/key\n  timeout: soon", "invalid duration"},
		"no playbooks":      {"playbooks: [site.yml]", "playbooks: []", "ansible.playbooks"},
		"bad git":           {"url: https://gitlab.example.org/", "url: https://gitlab.example.org/\n  type: bitbucket", "git.type"},
		"git.oauth unknown": {"trusted_group: myorg/sysadmins", "trusted_group: myorg/sysadmins\n  oauth:\n    client_id_file: /s/id", "oauth"},
		"no oidc":           {"  oidc:\n    issuer: https://auth.example.org/application/o/ansibled/\n    client_id_file: /s/oidc_id\n    client_secret_file: /s/oidc_secret\n    allowed_groups: [sysadmins]\n", "", "web.oidc.issuer"},
		"no issuer":         {"issuer: https://auth.example.org/application/o/ansibled/", "issuer: ''", "web.oidc.issuer"},
		"relative issuer":   {"issuer: https://auth.example.org/application/o/ansibled/", "issuer: auth.example.org", "web.oidc.issuer"},
		"ftp issuer":        {"issuer: https://auth.example.org/application/o/ansibled/", "issuer: ftp://auth.example.org/", "web.oidc.issuer"},
		"issuer query":      {"issuer: https://auth.example.org/application/o/ansibled/", "issuer: https://auth.example.org/?x=1", "web.oidc.issuer"},
		"no client id":      {"client_id_file: /s/oidc_id", "client_id_file: ''", "web.oidc.client_id_file: required"},
		"relative secret":   {"client_secret_file: /s/oidc_secret", "client_secret_file: s/oidc_secret", "web.oidc.client_secret_file: must be an absolute path"},
		"no groups":         {"allowed_groups: [sysadmins]", "allowed_groups: []", "web.oidc.allowed_groups"},
		"groups missing":    {"    allowed_groups: [sysadmins]\n", "", "web.oidc.allowed_groups"},
		"empty group":       {"allowed_groups: [sysadmins]", "allowed_groups: [sysadmins, '']", "empty group name"},
		"blank claim":       {"allowed_groups: [sysadmins]", "allowed_groups: [sysadmins]\n    groups_claim: ' '", "web.oidc.groups_claim"},
		"no openid scope":   {"allowed_groups: [sysadmins]", "allowed_groups: [sysadmins]\n    scopes: [profile, email]", "must include openid"},
		"bad scope":         {"allowed_groups: [sysadmins]", "allowed_groups: [sysadmins]\n    scopes: [openid, 'a b']", "not a valid scope"},
		"unknown oidc key":  {"allowed_groups: [sysadmins]", "allowed_groups: [sysadmins]\n    audience: x", "audience"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			src := strings.Replace(valid, tc.from, tc.to, 1)
			if src == valid {
				t.Fatalf("replacement %q not found", tc.from)
			}
			_, err := Parse([]byte(src))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestLoadAndReadSecret(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "ansibled.yml")
	if err := os.WriteFile(p, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil || c.Path != p {
		t.Fatalf("Load: %v %v", c, err)
	}
	s := filepath.Join(dir, "secret")
	os.WriteFile(s, []byte("  hunter2\n"), 0o600)
	if v, err := ReadSecret(s); err != nil || v != "hunter2" {
		t.Errorf("ReadSecret = %q, %v", v, err)
	}
	os.WriteFile(s, []byte("\n"), 0o600)
	if _, err := ReadSecret(s); err == nil {
		t.Error("empty secret accepted")
	}
}
