package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const valid = `
site: mysite
public_url: http://10.0.0.1:8471/
listen:
  webhook: 10.0.0.1:8470
  web: 10.0.0.1:8471
forge:
  url: https://gitlab.example.org/
  project: myorg/infra
  token_file: /s/token
  webhook_secret_file: /s/hook
  trusted_group: myorg/sysadmins
  oauth:
    client_id_file: /s/id
    client_secret_file: /s/secret
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
`

func TestParseValidAndDefaults(t *testing.T) {
	c, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if c.Forge.Type != "gitlab" || c.DataDir != "/var/lib/ansibled" {
		t.Errorf("defaults not applied: %+v", c)
	}
	if c.Ansible.Timeout.Duration != 45*time.Minute || c.Retention.Duration != 720*time.Hour {
		t.Errorf("duration defaults: %v %v", c.Ansible.Timeout, c.Retention)
	}
	if c.PublicURL != "http://10.0.0.1:8471" || c.Forge.URL != "https://gitlab.example.org" {
		t.Errorf("trailing slashes not trimmed: %q %q", c.PublicURL, c.Forge.URL)
	}
	if got := c.RunURL(7); got != "http://10.0.0.1:8471/runs/7" {
		t.Errorf("RunURL = %q", got)
	}
	if c.StatusName() != "ansibled/mysite" || c.CommentMarker() != "<!-- ansibled:site=mysite -->" {
		t.Errorf("names: %q %q", c.StatusName(), c.CommentMarker())
	}
	if c.Location().String() != "America/Los_Angeles" {
		t.Errorf("location %v", c.Location())
	}
}

func TestParseRejects(t *testing.T) {
	cases := map[string]struct{ from, to, want string }{
		"unknown key":    {"site: mysite", "site: mysite\nbogus: 1", "bogus"},
		"bad site":       {"site: mysite", "site: 'my site'", "site:"},
		"escaping path":  {"workdir: ansible", "workdir: ../x", "repo.workdir"},
		"abs inventory":  {"inventory: inventories/prod", "inventory: /etc/x", "ansible.inventory"},
		"check in args":  {"playbooks: [site.yml]", "playbooks: [site.yml]\n  extra_args: [--check]", "always set"},
		"relative token": {"token_file: /s/token", "token_file: s/token", "forge.token_file"},
		"bad cron":       {`"0 4 * * *"`, `"61 4 * * *"`, "schedule.nightly"},
		"bad tz":         {"America/Los_Angeles", "Mars/Olympus", "schedule.timezone"},
		"bad duration":   {"ssh_key_file: /s/key", "ssh_key_file: /s/key\n  timeout: soon", "invalid duration"},
		"no playbooks":   {"playbooks: [site.yml]", "playbooks: []", "ansible.playbooks"},
		"bad forge":      {"url: https://gitlab.example.org/", "url: https://gitlab.example.org/\n  type: bitbucket", "forge.type"},
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
