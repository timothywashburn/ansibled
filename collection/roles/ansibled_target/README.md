# timothywashburn.ansibled.ansibled_target

Prepares a host to be managed by [ansibled](https://github.com/timothywashburn/ansibled):

- a local **system** user (default `ansibled`) in `/etc/passwd`, never in a
  directory service, so ansibled can still log in when FreeIPA/LDAP is down
  (`nsswitch.conf` must list `files` first, the default). Home
  `/var/lib/ansibled` (0700), shell `/bin/bash`, password locked;
- `~/.ssh/authorized_keys` managed **exclusively** (other keys are removed), with
  `from="..."` when `ansibled_target_from` is set. Only `from=` is used: Ansible needs
  command execution without a pty plus sftp/scp, which the `restrict` options break;
- `/etc/sudoers.d/ansibled`: `NOPASSWD: ALL` (validated with `visudo -cf`, mode 0440;
  `sudo` must be installed);
- on SELinux hosts, the policy's default label on the home and `.ssh` directories
  (`ssh_home_t` for `/var/lib/*/.ssh`, which sshd needs). Keep the home where the
  policy labels `.ssh` that way.

Supported: EL 8/9/10 and Debian 12/13. Supports check mode.
`ansibled_target_state: absent` removes the user, its home and the sudoers file.

## Variables

| Variable | Default | Meaning |
|---|---|---|
| `ansibled_target_controller` | unset | Inventory hostname of the host running the ansibled daemon; its key is read from there. One of this and `ansibled_target_public_key` is required for `present` |
| `ansibled_target_controller_dir` | the controller's `ansibled_dir`, else `/opt/ansibled` | The `ansibled_dir` used on the controller |
| `ansibled_target_public_key` | unset | The daemon's SSH public key; when set it is used as is and the controller is not contacted |
| `ansibled_target_from` | `[]` | Addresses/CIDRs/patterns the key may be used from |
| `ansibled_target_user` | `ansibled` | Local user name (match `ansible.remote_user` in the ansibled config) |
| `ansibled_target_home` | `/var/lib/ansibled` | Home directory |
| `ansibled_target_state` | `present` | `present` or `absent` |

The key is read (as root, read-only, so also in check mode) from
`<ansibled_target_controller_dir>/etc/secrets/ssh_key.pub` on the controller
with `delegate_to`, so it works with `--limit` excluding the controller. Deploy
the `ansibled` role there first.

## Example

```yaml
- name: Hosts ansibled manages
  hosts: all
  become: true
  roles:
    - role: timothywashburn.ansibled.ansibled_target
      vars:
        ansibled_target_controller: "{{ groups['ansibled'][0] }}"
        ansibled_target_from: [10.0.0.1]
```

Alternatively give the key explicitly (it takes precedence):

```yaml
        ansibled_target_public_key: "ssh-ed25519 AAAA... ansibled"
```

If several ansibled instances manage the same host, put their keys in one
`ansibled_target_public_key` string, one per line.
