package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timothywashburn/ansibled/internal/config"
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
	c.Forge.OAuth.ClientIDFile = file("id", "i", 0o600)
	c.Forge.OAuth.ClientSecretFile = file("secret", "s", 0o600)
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
