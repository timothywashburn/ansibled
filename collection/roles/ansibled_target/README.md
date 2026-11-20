# timothywashburn.ansibled.ansibled_target

Prepares a host to be managed by [ansibled](https://github.com/timothywashburn/ansibled):

- a local **system** user (default `ansibled`) in `/etc/passwd`, never in a
  directory service, so ansibled can still log in when FreeIPA/LDAP is down. The
  role fails if a directory-service account with the same name exists. Home
  `/var/lib/ansibled` (0700), shell `/bin/bash`, password locked;
- `~/.ssh/authorized_keys` managed **exclusively** (other keys are removed), with
  `from="..."` when `ansibled_target_from` is set. Only `from=` is used: Ansible needs
  command execution without a pty plus sftp/scp, which the `restrict` options break;
- `/etc/sudoers.d/ansibled`: `NOPASSWD: ALL` (validated with `visudo -cf`, mode 0440);
  `sudo` is installed if missing;
- on hosts with SELinux enabled: `policycoreutils-python-utils`, an `ssh_home_t`
  file context for `<home>/.ssh(/.*)?` and `restorecon` when it changes.

Supported: EL 8/9/10 and Debian 12/13. Supports check mode.
`ansibled_target_state: absent` removes the user, its home, the sudoers file and
the file context.

## Variables

| Variable | Default | Meaning |
|---|---|---|
| `ansibled_target_public_key` | **required** for `present` | Public key string (one per line) or list of keys, without options |
| `ansibled_target_from` | `[]` | Addresses/CIDRs/patterns the key may be used from |
| `ansibled_target_user` | `ansibled` | Local user name (match `ansible.remote_user` in the ansibled config) |
| `ansibled_target_home` | `/var/lib/ansibled` | Home directory |
| `ansibled_target_state` | `present` | `present` or `absent` |
| `ansibled_target_manage_selinux` | `true` | Manage the `ssh_home_t` context when SELinux is enabled |

## Example

```yaml
- name: Hosts ansibled manages
  hosts: all
  become: true
  roles:
    - role: timothywashburn.ansibled.ansibled_target
      vars:
        # From the ansibled role in an earlier play, or paste the key.
        ansibled_target_public_key: "{{ hostvars[groups['ansibled'][0]].ansibled_public_key }}"
        ansibled_target_from: [10.0.0.1]
```

With several sites (one ansibled instance each), pass a list of their keys.
