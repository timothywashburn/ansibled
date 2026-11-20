package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timothywashburn/ansibled/internal/config"
	"github.com/timothywashburn/ansibled/internal/forge"
	"github.com/timothywashburn/ansibled/internal/forge/gitlab/gitlabtest"
	"github.com/timothywashburn/ansibled/internal/oidc/oidctest"
)

func TestReadSecretsSSHKey(t *testing.T) {
	dir := t.TempDir()
	file := func(name, content string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	c := &config.Config{}
	c.Forge.TokenFile = file("token", "t", 0o600)
	c.Forge.WebhookSecretFile = file("hook", "h", 0o600)
	c.Web.OIDC.ClientIDFile = file("id", "i", 0o600)
	c.Web.OIDC.ClientSecretFile = file("secret", "s", 0o600)
	c.Web.MetricsTokenFile = file("metrics", "m", 0o600)

	c.Ansible.SSHKeyFile = file("key", "-----BEGIN OPENSSH PRIVATE KEY-----\n", 0o600)
	if _, err := readSecrets(c); err != nil {
		t.Fatalf("readable key: %v", err)
	}

	c.Ansible.SSHKeyFile = file("empty", "", 0o600)
	if _, err := readSecrets(c); err == nil || !strings.Contains(err.Error(), "ssh_key_file") {
		t.Errorf("empty key: %v", err)
	}

	c.Ansible.SSHKeyFile = filepath.Join(dir, "missing")
	if _, err := readSecrets(c); err == nil || !strings.Contains(err.Error(), "ssh_key_file") {
		t.Errorf("missing key: %v", err)
	}

	if os.Geteuid() != 0 { // root reads anything
		c.Ansible.SSHKeyFile = file("unreadable", "key", 0o000)
		if _, err := readSecrets(c); err == nil || !strings.Contains(err.Error(), "ssh_key_file") {
			t.Errorf("unreadable key: %v", err)
		}
	}
}

func TestReadSecretsOIDC(t *testing.T) {
	dir := t.TempDir()
	file := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	c := &config.Config{}
	c.Forge.TokenFile = file("token", "t")
	c.Forge.WebhookSecretFile = file("hook", "h")
	c.Web.OIDC.ClientIDFile = file("oidc_client_id", "the-id\n")
	c.Web.OIDC.ClientSecretFile = file("oidc_client_secret", " the-secret ")
	c.Web.MetricsTokenFile = file("metrics", "m")
	c.Ansible.SSHKeyFile = file("key", "k")
	s, err := readSecrets(c)
	if err != nil || s.oidcID != "the-id" || s.oidcSecret != "the-secret" {
		t.Fatalf("readSecrets = %+v, %v", s, err)
	}
	c.Web.OIDC.ClientSecretFile = filepath.Join(dir, "missing")
	if _, err := readSecrets(c); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Errorf("missing OIDC secret: %v", err)
	}
}

func TestCheckConfigOnline(t *testing.T) {
	gl := gitlabtest.New()
	defer gl.Close()
	gl.AddGroupMember("myorg/sysadmins", forge.User{ID: 5, Username: "alice"})
	idp := oidctest.New()
	defer idp.Close()
	idp.SetIssuer(idp.URL() + "/application/o/ansibled/")

	dir := t.TempDir()
	sec := filepath.Join(dir, "secrets")
	os.MkdirAll(sec, 0o700)
	for name, v := range map[string]string{
		"forge_token": gitlabtest.Token, "webhook_secret": "w", "metrics_token": "m", "ssh_key": "k",
		"oidc_client_id": oidctest.ClientID, "oidc_client_secret": oidctest.ClientSecret,
	} {
		if err := os.WriteFile(filepath.Join(sec, name), []byte(v+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := filepath.Join(dir, "ansibled.yml")
	writeCfg := func(issuer string) {
		y := fmt.Sprintf(`site: lab
public_url: http://10.0.0.1:8471
listen: {webhook: "10.0.0.1:8470", web: "10.0.0.1:8471"}
forge:
  url: %s
  project: myorg/infra
  token_file: %[3]s/forge_token
  webhook_secret_file: %[3]s/webhook_secret
  trusted_group: myorg/sysadmins
repo: {setup: "true", venv_key_files: [uv.lock]}
ansible: {inventory: inv, playbooks: [site.yml], ssh_key_file: %[3]s/ssh_key}
data_dir: %[4]s
web:
  metrics_token_file: %[3]s/metrics_token
  oidc:
    issuer: %[2]s
    client_id_file: %[3]s/oidc_client_id
    client_secret_file: %[3]s/oidc_client_secret
    allowed_groups: [sysadmins]
`, gl.URL(), issuer, sec, filepath.Join(dir, "data"))
		if err := os.WriteFile(cfg, []byte(y), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	writeCfg(idp.Issuer())
	if err := checkConfig(cfg, false); err != nil {
		t.Fatalf("offline: %v", err)
	}
	if n := len(idp.Requests()); n != 0 {
		t.Errorf("offline check made %d OIDC requests", n)
	}
	if err := checkConfig(cfg, true); err != nil {
		t.Fatalf("online: %v", err)
	}
	if n := idp.CountRequests(oidctest.Discovery); n != 1 {
		t.Errorf("%d discovery requests", n)
	}

	writeCfg(idp.URL() + "/wrong/")
	err := checkConfig(cfg, true)
	if err == nil || !strings.Contains(err.Error(), "OIDC check failed") || strings.Contains(err.Error(), "forge check failed") {
		t.Errorf("online with wrong issuer: %v", err)
	}

	idp.EditDiscovery(func(d map[string]any) { delete(d, "userinfo_endpoint") })
	writeCfg(idp.Issuer())
	if err := checkConfig(cfg, true); err == nil || !strings.Contains(err.Error(), "userinfo_endpoint") {
		t.Errorf("online without userinfo endpoint: %v", err)
	}
}
