package engine

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/timothywashburn/ansibled/internal/model"
)

// ansibleEnv is the environment for ansible-playbook and ansible-galaxy
// (DESIGN.md "Run engine" step 6), without ANSIBLED_EVENTS_FILE.
// ANSIBLE_LOG_PATH is left unset (setting it to "" makes ansible warn that
// the working directory is not a file).
//
// Output is coloured (ANSIBLE_FORCE_COLOR) for the web UI's console; the raw
// log download strips the escapes. ansible.env can turn it off with
// ANSIBLE_NOCOLOR=1.
//
// ansible.env from the config is applied last, except that
// ANSIBLE_CALLBACK_PLUGINS and ANSIBLE_CALLBACKS_ENABLED from it are merged
// with ansibled's values so the events callback is always loaded.
func (rc *runCtxt) ansibleEnv() *env {
	ev := rc.baseEnv(rc.venv)
	ev.Set("ANSIBLE_PRIVATE_KEY_FILE", rc.cfg.Ansible.SSHKeyFile)
	if rc.cfg.Ansible.RemoteUser != "" {
		ev.Set("ANSIBLE_REMOTE_USER", rc.cfg.Ansible.RemoteUser)
	}
	ev.Set("ANSIBLE_FORCE_COLOR", "1")
	ev.Set("ANSIBLE_RETRY_FILES_ENABLED", "0")
	if rc.collections != "" {
		ev.Set("ANSIBLE_COLLECTIONS_PATH", rc.collections)
	}
	if rc.roles != "" {
		ev.Set("ANSIBLE_ROLES_PATH", filepath.Join(rc.workdir(), "roles")+":"+rc.roles)
	}

	cbPlugins, cbEnabled := rc.callbackDir, "ansibled_events"
	user := rc.cfg.Ansible.Env
	for _, k := range sortedKeys(user) {
		v := user[k]
		switch k {
		case "ANSIBLE_CALLBACK_PLUGINS":
			if v != "" {
				cbPlugins = v + ":" + cbPlugins
			}
		case "ANSIBLE_CALLBACKS_ENABLED", "ANSIBLE_CALLBACK_WHITELIST":
			if v != "" {
				cbEnabled = v + "," + cbEnabled
			}
		default:
			ev.Set(k, v)
		}
	}
	ev.Set("ANSIBLE_CALLBACK_PLUGINS", cbPlugins)
	ev.Set("ANSIBLE_CALLBACKS_ENABLED", cbEnabled)
	return ev
}

// playbook runs playbook number n and parses its events. A non-nil error is a
// context error or a *stepError for a condition that makes the result
// meaningless (the process could not start, or wrote no events); the
// remaining playbooks still run in the latter case.
func (rc *runCtxt) playbook(n int, pb string) (model.PlaybookResult, error) {
	res := model.PlaybookResult{Playbook: pb, ExitCode: -1, Hosts: []model.HostResult{}}
	events := rc.EventsPath(rc.run.ID, n)
	if err := os.Remove(events); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return res, stepErr("ansible", "cannot remove old events file")
	}
	ev := rc.ansibleEnv()
	ev.Set("ANSIBLED_EVENTS_FILE", events)

	bin := filepath.Join(rc.venv, "bin", "ansible-playbook")
	argv := append([]string{bin, "-i", rc.cfg.Ansible.Inventory, pb, "--check", "--diff"}, rc.cfg.Ansible.ExtraArgs...)
	display := strings.Join(append([]string{"ansible-playbook"}, argv[1:]...), " ")
	code, err := rc.exec(step{name: "ansible", display: display, argv: argv, dir: rc.workdir(), env: ev})
	res.ExitCode = code
	if rc.ctx.Err() != nil {
		return res, rc.ctx.Err()
	}
	if err != nil && code < 0 {
		return res, stepErr("ansible", "%s could not run", pb)
	}

	f, err := os.Open(events)
	if errors.Is(err, fs.ErrNotExist) {
		if code == 0 {
			fmt.Fprintf(rc.log, "no events file written for %s; is the ansibled_events callback in %s?\n", pb, rc.callbackDir)
			return res, stepErr("ansible", "%s: no events recorded (callback plugin not loaded?)", pb)
		}
		// Ansible stopped before the first play (parse error, missing
		// collection, bad inventory...): the reason is in the log.
		return res, stepErr("ansible", "%s: ansible-playbook exited %d before running any task (see the log)", pb, code)
	}
	if err != nil {
		return res, stepErr("ansible", "cannot read events for %s", pb)
	}
	defer f.Close()
	parsed, err := ParseEvents(f, pb, code)
	if err != nil {
		return res, stepErr("ansible", "cannot parse events for %s", pb)
	}
	return parsed, nil
}
