# timothywashburn.ansibled.ansibled

Runs the [ansibled](https://github.com/timothywashburn/ansibled) container on one
host (one instance per site): renders the config, writes the secrets, generates the
SSH key, writes the overlay and starts the container with Docker Compose.

## Requirements

On the daemon host:

- Docker with the Compose v2 plugin (`docker compose`), or Podman (see below).
- Python 3 for the Ansible modules, and `ssh-keygen` (or the Python `cryptography`
  library) for the key pair.
- An OpenID Connect provider (authentik, Keycloak, ...) with a confidential client
  for the web UI sign-in, redirect URI `<public_url>/oauth/callback` (see DESIGN.md
  "OIDC provider setup").

Open `listen.webhook` (for GitLab) and `listen.web` (for users and Prometheus) in your firewall yourself.

On the controller: the `community.docker`, `community.crypto`, `community.general`
and `ansible.posix` collections (installed as dependencies of this collection), and
`python-gitlab` if you use webhook registration.

## Host layout

```
/opt/ansibled/compose.yml                                       root 0644
/opt/ansibled/etc/            -> /etc/ansibled (read-only, :Z)  root:10001 0750
/opt/ansibled/etc/ansibled.yml                                  root:10001 0640
/opt/ansibled/etc/secrets/*   -> /etc/ansibled/secrets/*        10001 0400 (dir 0700)
/opt/ansibled/etc/overlay/... -> /etc/ansibled/overlay/...      10001 (dirs 0700)
/opt/ansibled/data/           -> /var/lib/ansibled (rw, :Z)     10001 0700
```

The container runs as `10001:10001` with host networking and `restart: always`,
`stop_grace_period: 60s`, and a healthcheck (`python3` + `urllib`, the image has no
curl) against `http://<listen.webhook>/healthz` (a wildcard bind is probed on
loopback).

## Variables

| Variable | Default | Meaning |
|---|---|---|
| `ansibled_config` | **required** | ansibled.yml settings, see below |
| `ansibled_image` | `ghcr.io/timothywashburn/ansibled:latest` | Container image |
| `ansibled_pull` | `always` | `always`, `missing` or `never` |
| `ansibled_container_name` | `ansibled-<site>` | Container name, also the Compose project name |
| `ansibled_dir` | `/opt/ansibled` | Host directory |
| `ansibled_uid` / `ansibled_gid` | `10001` | uid/gid the container runs as |
| `ansibled_ssh_key_comment` | `ansibled@<site>` | Comment of the generated SSH key |
| `ansibled_overlay_files` | `[]` | List of `{path, content \| src, mode}` copied over every checkout |
| `ansibled_overlay_prune` | `false` | Delete overlay files not in `ansibled_overlay_files` |
| `ansibled_gitlab_hook_token` | undefined | Enables webhook registration (Maintainer, `api`) |
| `ansibled_gitlab_hook_url` | `http://<listen.webhook>/webhook/gitlab` | URL GitLab posts to |
| `ansibled_gitlab_api_url` | `<git.url>` | GitLab base URL for the API |
| `ansibled_gitlab_validate_certs` | `true` | Verify TLS when talking to the GitLab API |
| `ansibled_gitlab_hook_validate_certs` | `true` | GitLab verifies the hook URL's TLS (https only) |

### `ansibled_config`

The keys of [ansibled.yml](../../../DESIGN.md#configuration) **without** the
container paths. The role merges these over your dict, so you never write them
(giving them anyway is an error):

```yaml
git: {token_file, webhook_secret_file}
ansible: {ssh_key_file}
overlay_dir, data_dir
web: {metrics_token_file, session_key_file, oidc: {client_id_file, client_secret_file}}
```

Instead, secrets are given inline (see [Secrets](#secrets)): `git.token`,
`git.webhook_secret`, `web.metrics_token`, `web.session_key`,
`web.oidc.client_id`, `web.oidc.client_secret`.

Required: `site`, `public_url`, `listen.webhook`, `listen.web`, `git.url`,
`git.project`, `git.trusted_group`, `repo.setup`, `repo.venv_key_files`,
`ansible.inventory`, `ansible.playbooks`, `web.oidc.issuer`,
`web.oidc.allowed_groups`. Optional: `git.type`, `git.clone_url`,
`repo.workdir`, `repo.collections_requirements`, `ansible.extra_args`,
`ansible.remote_user`, `ansible.timeout`, `ansible.env`, `schedule.nightly`,
`schedule.timezone`, `retention`, `web.oidc.groups_claim` (default `groups`),
`web.oidc.scopes` (default `[openid, profile, email]`, must include `openid`). Unknown keys fail early
(`meta/argument_specs.yml` and an assert), because ansibled rejects them too.

## Secrets

Secrets go inline in `ansibled_config`. The role writes each one to a file under
`secrets/` (0400, uid 10001) and leaves it out of the rendered `ansibled.yml`:

| Key | When not given | Meaning |
|---|---|---|
| `git.token` | first deploy fails; later the file on the host is kept | GitLab project access token (Developer; `api`, `read_repository`) |
| `web.oidc.client_id` | same | OpenID Connect client id (web UI sign-in) |
| `web.oidc.client_secret` | same | OpenID Connect client secret |
| `git.webhook_secret` | generated once, then kept | Webhook secret token |
| `web.metrics_token` | generated once, then kept | Bearer token for `/metrics` |
| `web.session_key` | generated once, then kept | Web session signing key |

- Giving a key later replaces the file (and restarts the container).
- If a required key is missing and its file does not exist yet, the role fails
  naming the key (in check mode it only prints a warning, so a dry run of a fresh
  host does not fail).
- The repo that deploys ansibled is usually the repo ansibled dry-runs, so anyone
  who can read that repo can read whatever secrets you commit to it. Leave
  `git.webhook_secret` out (generated on the host): whoever knows it can forge
  "a trusted user pushed X" webhooks and get code run as root on the site.
- Every task that handles a secret uses `no_log: true`.
- Do **not** add the token's bot user to the trusted group (ansibled ignores the
  bot as a trigger anyway). It only needs to be able to *see* the group's members.

After the secrets are written, the role sets these facts (no_log) for later plays:

| Fact | Content |
|---|---|
| `ansibled_public_key` | The daemon's SSH public key, for `ansibled_target` |
| `ansibled_webhook_secret_effective` | The webhook secret on the host |
| `ansibled_metrics_token_effective` | The metrics token on the host (e.g. for a Prometheus `credentials_file`) |

## Restarts

ansibled reads its config and secrets at startup, so a change to `ansibled.yml` or
a secret restarts the container (handler; skipped when `docker compose up` already
recreated it in the same run). A change of image or `compose.yml` recreates it.
Overlay changes need no restart: ansibled copies the overlay into every checkout.
Changing the SSH key comment needs no restart either.

## Webhook registration

With `ansibled_gitlab_hook_token` defined, the role makes sure the project has a hook
at `ansibled_gitlab_hook_url` with push, merge request and comment (note) events and
the effective webhook secret as its token. It runs on the controller
(`delegate_to: localhost`, `become: false`) and needs `python-gitlab` there. It is
not `run_once`: every daemon host in the play registers its own URL (one per site).

GitLab never returns a hook's secret, so `community.general.gitlab_hook` would
report a change on every run. The role therefore first lists the hooks (read-only,
also in check mode) and only calls `gitlab_hook` when the hook is missing, its
events or TLS setting differ, or the webhook secret changed in this run. Keep the
hook token out of the repo as well.

## Check mode

The role supports `--check` (ansibled itself dry-runs repos that contain it). On a
fresh host nothing exists yet, so the key pair, `docker compose` and the facts that
depend on files on the host are skipped; on later runs a check run reports no
changes.

## Podman hosts

This works on rootful Podman with `podman-docker` (the `docker` command, and the
`/run/docker.sock` link to the Podman socket) plus the Docker Compose plugin talking
to the Podman socket: `systemctl enable --now podman.socket`. Podman does not
restart containers itself: containers with `restart: always` come back after a
reboot through `podman-restart.service`, which must be enabled.

## Example

```yaml
# requirements.yml
collections:
  - name: https://github.com/timothywashburn/ansibled.git#/collection
    type: git
    version: main
```

```yaml
- name: ansibled daemon
  hosts: ansibled
  become: true
  roles:
    - role: timothywashburn.ansibled.ansibled
      vars:
        ansibled_config:
          site: mysite
          public_url: http://10.0.0.1:8471
          listen:
            webhook: 10.0.0.1:8470
            web: 10.0.0.1:8471
          git:
            url: https://gitlab.example.org
            project: myorg/infra
            trusted_group: myorg/sysadmins
            token: "{{ vault_ansibled_git_token }}"
          repo:
            workdir: ansible
            setup: uv sync --frozen --no-dev
            venv_key_files: [pyproject.toml, uv.lock]
            collections_requirements: requirements.yml
          ansible:
            inventory: inventories/mysite
            playbooks: [site.yml]
            remote_user: ansibled
          schedule:
            nightly: "0 4 * * *"
            timezone: America/Los_Angeles
          web:
            oidc:
              issuer: https://auth.example.org/application/o/ansibled/
              client_id: "{{ vault_ansibled_oidc_client_id }}"
              client_secret: "{{ vault_ansibled_oidc_client_secret }}"
              allowed_groups: [sysadmins]
        ansibled_overlay_files:
          - path: ansible/.vault-pass
            content: "{{ lookup('ansible.builtin.file', '~/.ansibled-vault-pass') }}"
            mode: "0400"

- name: Hosts ansibled manages
  hosts: all
  become: true
  roles:
    - role: timothywashburn.ansibled.ansibled_target
      vars:
        ansibled_target_public_key: "{{ hostvars[groups['ansibled'][0]].ansibled_public_key }}"
        ansibled_target_from: [10.0.0.1]
```
