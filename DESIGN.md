# ansibled design

ansibled ("ansible daemon") watches one git repository and dry-runs its Ansible
playbooks (`ansible-playbook --check --diff`) against one site:

- on merge requests, when a trusted person pushes or asks for it, and reports back on the MR;
- on every new commit to the default branch;
- on a schedule (nightly), to detect drift between the repo and the real hosts.

It is a single Go binary in a container. Ansible itself runs as a child process
inside the same container, using the Python environment and collections the
checked-out commit asks for.

This document is the source of truth for behaviour. If code and this document
disagree, one of them is a bug.

## Scope

- **One instance = one site**: one repo, one inventory, one or more playbooks.
  A repo that manages several sites runs one instance per site, each placed where
  it can reach its hosts.
- **Dry runs only** (`--check --diff`). Real applies are a later feature; the run
  record already carries a `mode` field so they can be added without a schema change.
- **GitLab first.** Everything forge-specific is behind the `forge.Forge`
  interface so a GitHub adapter can be added later without touching the core.

Non-goals: a general CI system, running arbitrary commands, multi-tenant hosting.

## Trust model

A dry run is not a sandbox. `--check` runs with become (root) on every host, and
playbook code can opt out of check mode (`check_mode: false`), run code on the
controller (`delegate_to: localhost`, lookups), or change `ansible.cfg`.
**Running a commit is equivalent to giving its author root on the site.**

Therefore ansibled only runs code that a member of the configured **trusted
group** (a GitLab group, e.g. `myorg/sysadmins`) has vouched for:

| Trigger | Runs when |
|---|---|
| Push to an MR's source branch | The person who **pushed** that commit (from the push webhook, which GitLab fills in server side and which cannot be spoofed by commit metadata) is in the trusted group. |
| MR opened / reopened | The person who pushed the MR's head commit (looked up via the project events API) is in the group. |
| `/ansibled check` comment on an MR | The **comment author** is in the group. Runs the MR's current head. Works for MRs from forks too, so a sysadmin can run a reviewed outside contribution. |
| Push to the default branch | The pusher (usually whoever clicked merge) is in the group. |
| Nightly schedule | Always (runs the default branch head, which already passed the rule above). |
| Startup catch-up | Same rules as above, using the events API to find pushers. |

Everything else is ignored and counted in `ansibled_webhooks_total{accepted="false"}`.

ansibled's own bot user (the git token's user) is **never** trusted, even if it
is a member of the group: the token can push commits, so trusting it would turn
a leaked token into root on the site.

The rule rests on the webhook being authentic: the pusher and comment author come
from the webhook body, and only the webhook secret proves GitLab sent it.
**Anyone who can read the webhook secret can get any commit run.** Keep it out of
the repository being checked (even a private one) and only on the ansibled
host. The deploy role generates the webhook secret on the host for this reason.

The log page and `/metrics` always require authentication (see [Web UI](#web-ui-and-auth)).

## Triggers and run kinds

| Kind | Key | Commit | Reported to |
|---|---|---|---|
| `mr` | `mr:<iid>` | MR head | commit status + one edited MR comment |
| `main` | `main` | default branch head | commit status only (never an MR comment) |
| `nightly` | `nightly` | default branch head | metrics + log page only |

`main` and `nightly` runs both update the drift metrics. `mr` runs never do,
because an MR is not what is deployed.

### Webhooks (GitLab)

One endpoint, `POST /webhook/gitlab`, on the webhook listener. The project
webhook must send **push**, **merge request** and **comment (note)** events.

- Verify `X-Gitlab-Token` against the configured secret with a constant-time
  compare. Wrong/missing → `401`, nothing else happens.
- Body limit 5 MiB. Respond `202` immediately; process asynchronously.
- **Push Hook**: ignore tag pushes and branch deletions (`after` all zeros).
  - Default branch → `main` run for `after`.
  - Other branch → for each open MR in *this* project whose source branch is
    that branch (forks never send push hooks here), an `mr` run for `after`.
- **Merge Request Hook**:
  - `open`, `reopen` → `mr` run for `last_commit.id`, if its pusher is trusted.
  - `update` with new commits → ignored (the push hook already handled it).
  - `close`, `merge` → cancel queued/running runs for `mr:<iid>`.
- **Note Hook** on a merge request: if the body, trimmed, starts with
  `/ansibled check` and the author is trusted and is not ansibled's own bot user
  → `mr` run for the MR's current head (fetched from the API, not the payload).

### Startup catch-up

Webhooks sent while ansibled is down are lost (GitLab does not redeliver). On
every start, after re-queueing interrupted runs:

1. Default branch head: if no `main` run exists for that sha → queue one (subject
   to the pusher rule via the events API; if the pusher can't be found, still
   queue it, because the default branch is protected and reviewed).
2. Every open MR in this project: if no `mr` run exists for its head sha and the
   head's pusher is trusted → queue one.

This is a one-off sync, not polling.

### Nightly

`schedule.nightly` is a 5-field cron expression evaluated in `schedule.timezone`.
If a `main` run for the current head is already queued or running when it fires,
the nightly run is skipped (that run updates the same metrics).

## Queue

- One worker: at most one ansible run at a time per instance (all runs hit the
  same hosts).
- FIFO across keys.
- **Supersede**: queueing a run with key K cancels any queued or running run with
  key K. A running run is cancelled by SIGTERM to its process group, then SIGKILL
  after 15 s. The cancelled run gets status `canceled` with description
  "superseded by <short sha>".
- **Dedupe**: queueing (K, sha) is a no-op if a run for (K, sha) is already
  queued, running, or finished (unless the trigger is a `/ansibled check` comment,
  which always runs).
- **Cancel**: the web UI cancels one run by id (`CancelRun`), with
  description "canceled by <user>".
- Queue state lives in SQLite, so it survives restarts. On startup, runs that
  were `running` are reset to `queued` and run again.
- Per-run timeout: `ansible.timeout` (default 45m) → state `error`.

## Run engine

Each run gets `data_dir/runs/<id>/`:

1. **Fetch.** A bare repo at `data_dir/repo.git` is fetched from the forge over
   HTTPS. Credentials go in through `GIT_CONFIG_COUNT`/`GIT_CONFIG_KEY_n`/
   `GIT_CONFIG_VALUE_n` env vars (an `http.extraHeader`), never in argv.
   - `mr` → `refs/merge-requests/<iid>/head`, `main`/`nightly` → `refs/heads/<default>`.
   - The exact sha must exist after the fetch, otherwise `error`.
2. **Checkout.** `git archive <sha> | tar -x` into `runs/<id>/checkout/`. No
   `.git` in the checkout, no worktree bookkeeping. (Submodules and LFS are not
   supported.)
3. **Overlay.** Every regular file and directory under `overlay_dir` is copied
   over the checkout at the same path relative to the **repo root**, keeping
   file modes. Symlinks in the overlay are skipped with a warning. This is how
   gitignored files (secrets, state markers, become-password files) get in.
4. **Python environment.** `repo.venv_key_files` (relative to `repo.workdir`,
   e.g. `[pyproject.toml, uv.lock]`) are hashed (sha256 over path + content).
   The venv lives at `data_dir/venvs/<hash>/`. If it has no `.ansibled-ok`
   marker, `repo.setup` runs with `sh -c` in the workdir, with:
   - `ANSIBLED_VENV` and `UV_PROJECT_ENVIRONMENT` = the venv path
   - `UV_CACHE_DIR` = `data_dir/cache/uv`
   On success the marker is written. Examples:
   - uv project: `uv sync --frozen --no-dev`
   - pip requirements: `uv venv "$ANSIBLED_VENV" && uv pip install -p "$ANSIBLED_VENV" -r requirements.txt`
5. **Collections.** If `repo.collections_requirements` exists in the checkout,
   it is hashed the same way and installed with the venv's `ansible-galaxy
   collection install -r <file> -p data_dir/collections/<hash>` (unless marked
   done). `ANSIBLE_COLLECTIONS_PATH` is set to that path. Roles in the file are
   installed to `data_dir/roles/<hash>` and put on `ANSIBLE_ROLES_PATH` after the
   repo's own.
6. **Ansible.** For each playbook in order, in the workdir:
   ```
   <venv>/bin/ansible-playbook -i <inventory> <playbook> --check --diff <extra_args...>
   ```
   Environment (on top of a minimal base env, not ansibled's own):
   - `HOME=data_dir/home` (persistent `~/.ssh/known_hosts`, uv cache)
   - `ANSIBLE_PRIVATE_KEY_FILE`, and `ANSIBLE_REMOTE_USER` if `ansible.remote_user` is set
   - `ANSIBLE_CALLBACK_PLUGINS=<callback dir>` (default `/usr/share/ansibled/callback_plugins`, overridable with the `ANSIBLED_CALLBACK_DIR` env var of the daemon), appended to any existing value
   - `ANSIBLE_CALLBACKS_ENABLED=ansibled_events`
   - `ANSIBLED_EVENTS_FILE=runs/<id>/events-<n>.jsonl` (one file per playbook, `n` = index in `ansible.playbooks`)
   - `ANSIBLE_FORCE_COLOR=1` (the web console renders the colours; the raw log
     download strips them; `ANSIBLE_NOCOLOR=1` in `ansible.env` turns them off),
     `ANSIBLE_RETRY_FILES_ENABLED=0`, `ANSIBLE_LOG_PATH=` (unset)
   - `ansible.env` from config last
   stdout+stderr of every step go to `runs/<id>/log.txt` with a header line per step.
   The process runs in its own process group (`Setpgid`) so cancellation reaches ssh children.
7. **Parse.** Each `events-<n>.jsonl` plus that playbook's exit code produce the run summary (below).
8. **Clean.** `checkout/` is deleted. `log.txt` and `events.jsonl` stay until
   retention removes them.

The whole run (all steps) is bounded by `ansible.timeout`. The engine holds an
exclusive `flock` on `data_dir/engine.lock` while running, so `run-once` and the
daemon never run at the same time. Every playbook runs even if an earlier one
failed.

### Callback plugin

`callback/ansibled_events.py`, installed in the image at
`/usr/share/ansibled/callback_plugins/`. `CALLBACK_TYPE = "aggregate"` so the
default stdout stays the human-readable log. It writes one JSON object per line:

```json
{"event":"play_start","playbook":"site.yml","play":"compute + login nodes"}
{"event":"task_start","play":"...","task":"docker : podman restart service","path":"roles/docker/tasks/main.yml:51"}
{"event":"result","status":"ok|changed|failed|unreachable|skipped","host":"web1","play":"...","task":"...","action":"ansible.builtin.copy","ignore_errors":false,"msg":"...","diff":"--- before\n+++ after\n..."}
{"event":"stats","hosts":{"web1":{"ok":30,"changed":3,"failures":0,"unreachable":0,"skipped":4,"rescued":0,"ignored":0}}}
```

It only hooks task-level `v2_runner_on_*` results (not per loop item), plus
`v2_playbook_on_play_start`, `v2_playbook_on_task_start`,
`v2_playbook_on_handler_task_start` and `v2_playbook_on_stats`. `msg` is
truncated to 4000 characters. `changed` results carry `diff`: the `--diff`
output of the task and of its changed loop items, rendered as ansible prints it
(`CallbackBase._get_diff`) without colour, truncated to 64 KiB. The web UI's
Changes tab reads these files back (`engine.ParseDetails`). It must work on
ansible-core 2.15+.

### Result

Ansible exit codes: `0` ok, `2` task failures, `4` unreachable hosts, anything
else is an error. Per run:

| State | Meaning |
|---|---|
| `success` | Every playbook exited 0. Changes are allowed: in check mode a "change" means "would change". |
| `failed` | At least one host had a failed (not ignored) task or was unreachable. |
| `error` | Setup, fetch, a playbook exiting with another code, or the timeout. |
| `canceled` | Superseded, MR closed/merged, or shutdown. |

Summary, per playbook and host: `changed`, `failed`, `unreachable`, plus the list
of task names that would change and that failed. Stored as JSON in the run record.

## Reporting

### Commit status (`mr` and `main` runs)

- Name: `ansibled/<site>`, so several sites show side by side on the same commit
  ("1 of 2 passed").
- `pending` when queued, `running` when started, then `success`/`failed`/`canceled`;
  `error` is reported as `failed`.
- Description (≤ 140 chars): e.g. `would change 4 tasks on 2 hosts`, `2 failed on db1`,
  `error: setup command failed`, `superseded by a1b2c3d`.
- `target_url`: `<public_url>/runs/<id>`.
- `ref`: the branch name.

### MR comment (`mr` runs only)

One comment per site per MR, found by the hidden marker
`<!-- ansibled:site=<site> -->` in a note authored by ansibled's bot user, and
edited in place on every run (created if missing). It contains **no diffs and
no task messages** (every project member can read comments), only counts and
task names:

```markdown
<!-- ansibled:site=mysite -->
### ✅ ansibled · mysite · dry run passed
`a1b2c3d` · 3m12s · [full log](http://10.0.0.1:9251/runs/412)

| host | would change | failed | unreachable |
|---|---:|---:|:-:|
| web1 | 3 | 0 | |
| db1 | 1 | 0 | |

<details><summary>Would change (4)</summary>

- web1: nginx : deploy site config
- …
</details>
```

States: ⏳ queued/running (shows the sha being run), ✅ passed, ❌ failed,
⚠️ error (with the failing step), ⏹ canceled (with the superseding sha). With
more than one playbook, one table per playbook.

### Metrics

Exposed on the web listener at `GET /metrics`, requiring
`Authorization: Bearer <token>` (Prometheus: `authorization.credentials_file`).

| Metric | Type | Notes |
|---|---|---|
| `ansibled_drift_changed_tasks{playbook,host}` | gauge | from the latest finished `main`/`nightly` run |
| `ansibled_drift_failed_tasks{playbook,host}` | gauge | same |
| `ansibled_drift_unreachable{playbook,host}` | gauge | 1/0, same |
| `ansibled_drift_run_timestamp_seconds` | gauge | when the drift numbers were last updated |
| `ansibled_last_run_timestamp_seconds{kind}` | gauge | any finish |
| `ansibled_last_run_success{kind}` | gauge | 1 if state was `success` |
| `ansibled_runs_total{kind,state}` | counter | |
| `ansibled_run_duration_seconds{kind}` | histogram | |
| `ansibled_queue_length` | gauge | |
| `ansibled_webhooks_total{event,accepted}` | counter | rejected = bad token, untrusted, ignored |

Drift gauges for a playbook are reset before each update so removed hosts disappear.
Gauges are restored from the last finished run in SQLite on startup.
Example alert rules live in `docs/alerts.yml`; deployments own their real rules.

## Web UI and auth

Two listeners (they may be the same address):

- `listen.webhook`: only `POST /webhook/<forge>` and `GET /healthz`.
- `listen.web`: the UI pages below, `/static/<file>`, `/login`,
  `/oauth/callback`, `/logout`, `/metrics`, `/healthz`.

Binding retries every 5 s until the address exists (it may be on an interface
that comes up after the container, e.g. a VPN), instead of exiting.

Pages (all behind sign-in):

| path | what |
|---|---|
| `/` | Overview: latest main and nightly result, drift per host (from the last nightly run, or main until there is one), active runs, recent runs. |
| `/runs?kind=&state=&mr=&page=` | Run list with filters, 50 per page. |
| `/runs/<id>` | Run page: status, ref, commit and MR links (when the forge provides them), trigger, duration; tabs **Summary** (per-host counts), **Changes** (per host, the tasks that would change with their diffs and the tasks that failed with their messages) and **Log** (the live console). Re-run and Cancel buttons. |
| `/runs/<id>/stream` | Server-Sent Events: `run` (state, at the start and on every change), `log` (new complete lines, `id:` = byte offset, so a reconnecting browser resumes with `Last-Event-ID`), `done`. The log file is polled every 250 ms and the run every second. A first connection to a log larger than 2 MiB starts at its last 2 MiB. |
| `/runs/<id>/log.txt` | Raw log without colour codes; `?ansi=1` keeps them, `?download=1` makes it an attachment. |
| `POST /runs/<id>/rerun`, `POST /runs/<id>/cancel` | Buttons. Re-run queues the run's kind again (trigger `web`, forced): an MR run at the MR's current head (refused if the MR is not open), a main or nightly run at the current head of the default branch, so an old commit never overwrites newer results. Cancel cancels that one queued or running run ("canceled by <user>"). |
| `POST /run/main` | "Run main" button on the overview and the run list: queues a main run (trigger `web`, forced) of the default branch's current head, which sets its commit status and drift metrics like any main run. |

Pages are server-rendered with `html/template`; one embedded script
(`internal/web/static/app.js`, no dependencies) adds the console, tabs, live
refresh of the overview and run list every 5 s, and relative times. Without
JavaScript every page still renders, and the log is available raw. The CSP
allows only same-origin scripts, styles and connections. Forms carry a CSRF
token (an HMAC of the session) and are refused when the browser labels them
cross-site (`Sec-Fetch-Site`). Anyone allowed to sign in may press the buttons.

The console: one section per engine step (collapsible, with exit code and
duration; successful steps other than `ansible` collapse when they finish),
ansible's ANSI colours, line numbers, search, wrap, copy and download. It
follows the end of the log; scrolling up pauses that and shows a "Follow"
button.

Sign-in uses **generic OpenID Connect** (`internal/oidc`), so any provider works
(authentik, Keycloak, Google, ...). It is independent of the forge: who may
*read logs* is decided by the provider's groups claim (`web.oidc.allowed_groups`),
who may *trigger runs* stays the forge's trusted group (see [Trust model](#trust-model)).

Discovery: `GET <issuer without trailing slash>/.well-known/openid-configuration`,
on first use (not at startup, so a provider outage never stops the daemon), with a
30 s timeout. A successful result is cached for the life of the process; a failure
is not, the next sign-in retries. The document must have `authorization_endpoint`,
`token_endpoint` and `userinfo_endpoint`, and its `issuer` must equal
`web.oidc.issuer` (ignoring one trailing slash). A plain-http issuer is allowed
(tests, labs) with a warning.

Flow (authorization code with PKCE `S256` and `state`, confidential client):

1. `/login` → random `state` and PKCE verifier in a signed, 10-minute cookie →
   redirect to the provider's `authorization_endpoint` with `response_type=code`,
   `web.oidc.scopes`, the `code_challenge` and redirect URI
   `<public_url>/oauth/callback`. If discovery fails: 502 "Could not reach the
   sign-in provider."
2. `/oauth/callback` → check `state`, exchange the code at the `token_endpoint`
   (client secret via HTTP basic auth, with the `code_verifier`), then `GET
   userinfo_endpoint` with `Authorization: Bearer <access token>`. All tokens are
   discarded afterwards. ID tokens are **not** validated: ansibled trusts the
   userinfo answer, which it fetched itself, server side, over the provider's TLS.
   Exchange or userinfo failure: 502.
3. From the userinfo claims: subject = `sub` (required), username =
   `preferred_username`, else `email`, else `sub`; groups = the
   `web.oidc.groups_claim` claim (default `groups`), a JSON array of strings
   (non-strings ignored) or a single string, missing = none. A user in none of
   `web.oidc.allowed_groups` (exact, case-sensitive match) gets a 403 page
   "<username> is not in an allowed group (<allowed groups>)."
4. Set a session cookie holding a payload version, the OIDC subject and the
   username (HMAC-SHA256 signed with `web.session_key_file`, or a random key per
   process if unset; `HttpOnly`, `SameSite=Lax`, `Secure` when `public_url` is
   https), valid for 12 h. Cookies of another payload version (e.g. from the
   GitLab sign-in era) are rejected. Group membership is checked at sign-in only.

Cookie names include the site (`ansibled_session_<site>`, `ansibled_oauth_<site>`)
so several instances on one host don't collide. Errors and logs never contain
tokens or the client secret. Expired runs (past retention) keep their summary,
but their log and Changes details are gone.

## Forge interface

```go
type Forge interface {
    Name() string                                    // "gitlab"
    HandleWebhook(r *http.Request) ([]Event, error)  // verifies secret, parses
    DefaultBranch(ctx) (string, error)
    BranchHead(ctx, branch string) (string, error)
    OpenMRs(ctx) ([]MR, error)                       // this project's open MRs
    MRsForBranch(ctx, branch string) ([]MR, error)
    GetMR(ctx, iid int) (MR, error)
    PusherOf(ctx, branch, sha string) (*User, error) // nil if unknown
    IsTrusted(ctx, u User) (bool, error)             // trusted-group membership, cached 60 s
    BotUser(ctx) (User, error)                       // the token's own user
    SetStatus(ctx, sha string, s Status) error
    UpsertMRComment(ctx, iid int, marker, body string) error
    GitAuth() (fetchURL string, header string)       // HTTPS URL + Authorization header value
    MRRef(iid int) string                            // "refs/merge-requests/<iid>/head"
}
```

GitLab endpoints used (API v4, project addressed by URL-encoded path):
`GET /projects/:id`, `GET /projects/:id/repository/branches/:b`,
`GET /projects/:id/merge_requests?state=opened[&source_branch=]`,
`GET /projects/:id/merge_requests/:iid`, `GET /projects/:id/events?action=pushed`,
`GET /groups/:group/members/all/:user_id`, `GET /user`,
`POST /projects/:id/statuses/:sha`, `GET|POST|PUT /projects/:id/merge_requests/:iid/notes`.

### GitLab setup (per site)

- **Project access token**, role *Developer*, scopes `api`, `read_repository`.
  Its bot user must be able to see the trusted group's members: give the group
  *internal* visibility. Do **not** add the bot to the trusted group (it would
  be ignored anyway). Verify with `ansibled check-config --online`.
- **Webhook** to `http://<listen.webhook>/webhook/gitlab` with a secret token,
  events: push, merge request, comments. The deploy role can create it.

GitLab is not used for the web UI sign-in; see the next section.

### OIDC provider setup

Any OpenID Connect provider with discovery, the authorization code flow, PKCE,
a userinfo endpoint and a groups claim works. With **authentik** as the example:

1. *Applications → Providers → Create → OAuth2/OpenID Provider*:
   - client type **Confidential**;
   - redirect URI (strict) `<public_url>/oauth/callback`, e.g.
     `http://10.0.0.1:9251/oauth/callback`;
   - scopes: the default `openid`, `profile` and `email` mappings. authentik's
     default *profile* scope mapping already puts the user's group names into a
     `groups` claim, so `web.oidc.groups_claim` can stay at its default.
2. *Applications → Create*, slug e.g. `ansibled`, with that provider. The issuer
   is then `https://auth.example.org/application/o/ansibled/` (shown on the
   provider page as "OpenID Configuration Issuer").
3. Put the client id and client secret into the files named by
   `web.oidc.client_id_file` / `client_secret_file` (the deploy role writes them
   from `ansibled_oidc_client_id` / `ansibled_oidc_client_secret`), and list the
   authentik groups that may read logs in `web.oidc.allowed_groups`.
4. Verify with `ansibled check-config --online` (runs discovery).

Other providers: Keycloak (issuer `https://<host>/realms/<realm>`; add a *Group
Membership* mapper to the client with token claim name `groups`, *Full group path*
off and *Add to userinfo* on). Providers without a groups claim (e.g. plain Google
accounts) cannot be used, since access is decided by groups.

## Configuration

One YAML file (default `/etc/ansibled/ansibled.yml`). Secrets are only ever
referenced by file path. Unknown keys are an error.

```yaml
site: mysite                       # status name ansibled/<site>, comment marker
public_url: http://10.0.0.1:9251    # base for links and the sign-in redirect (<public_url>/oauth/callback)

listen:
  webhook: 10.0.0.1:9250
  web: 10.0.0.1:9251

git:
  type: gitlab
  url: https://git.example.org
  project: myorg/infra              # path or numeric id
  token_file: /etc/ansibled/secrets/git_token
  webhook_secret_file: /etc/ansibled/secrets/webhook_secret
  trusted_group: myorg/sysadmins     # who may trigger runs

repo:
  workdir: ansible                  # ansible project dir relative to repo root ("." default)
  setup: uv sync --frozen --no-dev
  venv_key_files: [pyproject.toml, uv.lock]
  collections_requirements: requirements.yml   # optional

ansible:
  inventory: inventories/mysite
  playbooks: [site.yml]
  extra_args: []
  remote_user: ansibled             # optional
  ssh_key_file: /etc/ansibled/secrets/ssh_key
  timeout: 45m
  env: {}

overlay_dir: /etc/ansibled/overlay  # optional
data_dir: /var/lib/ansibled

schedule:
  nightly: "0 4 * * *"              # optional; omit to disable
  timezone: America/Los_Angeles

retention: 720h                     # run logs; run records are kept

web:
  metrics_token_file: /etc/ansibled/secrets/metrics_token
  session_key_file: /etc/ansibled/secrets/session_key   # optional
  oidc:                                                  # web UI sign-in (who may read logs)
    issuer: https://auth.example.org/application/o/ansibled/   # absolute http(s) URL
    client_id_file: /etc/ansibled/secrets/oidc_client_id
    client_secret_file: /etc/ansibled/secrets/oidc_client_secret
    allowed_groups: [sysadmins]     # required, non-empty; a user must be in at least one
    groups_claim: groups            # optional, default "groups"
    scopes: [openid, profile, email]   # optional, default this; must include openid
```

## CLI

- `ansibled serve [-config path]`: the daemon (default command).
- `ansibled check-config [-config path] [--online]`: validate the config and read
  every secret file (including the SSH key); `--online` also checks the git
  token, project and trusted-group visibility, and runs OIDC discovery.
- `ansibled run-once [-config path] --sha <sha>|--branch <b>`: run the engine
  once in the foreground, print the summary, write nothing to the forge.

Logs are JSON (`log/slog`) on stdout.

## Storage

`data_dir/`:

```
ansibled.db              SQLite (modernc.org/sqlite, no cgo)
repo.git/                bare repo
venvs/<hash>/            Python environments, reused across runs
collections/<hash>/      installed collections
roles/<hash>/            installed galaxy roles
cache/uv/                uv download cache
home/                    HOME for ansible (known_hosts)
runs/<id>/               log.txt, events-<n>.jsonl (checkout/ only while running)
```

Tables: `runs(id, kind, key, mode, sha, ref, mr_iid, trigger, triggered_by,
state, description, summary_json, queued_at, started_at, finished_at,
logs_expired)`. The MR comment is found by its marker each time (the GitLab
adapter may cache note ids in memory).

Housekeeping once an hour: delete `runs/<id>/` files older than `retention`
(mark `logs_expired`), delete venvs/collections/roles not used for 14 days.

## Container image

`ghcr.io/<owner>/ansibled`, built by GitHub Actions for `linux/amd64` and
`linux/arm64`. Every push to `main` publishes `:latest` and `:sha-<short>`.
Pull requests build and test but don't push.

- Build stage: `golang`, static binary (`CGO_ENABLED=0`).
- Runtime: `python:3.13-slim` plus `uv`, `git`, `openssh-client`, `tini`,
  `ca-certificates`. Runs as uid 10001 `ansibled`.
- `ENTRYPOINT ["tini", "--", "ansibled"]`, `CMD ["serve"]`. tini reaps the ssh
  processes Ansible leaves behind.
- Volumes: `/etc/ansibled` (config, secrets, overlay; read-only),
  `/var/lib/ansibled` (data).

## Ansible collection

`collection/` is the `timothywashburn.ansibled` collection, installable straight
from git:

```yaml
collections:
  - name: https://github.com/timothywashburn/ansibled.git#/collection
    type: git
    version: main
```

### Role `ansibled` (the daemon host)

- Creates `ansibled_dir` (default `/opt/ansibled`) with `etc/ansibled.yml`,
  `etc/secrets/`, `etc/overlay/` (mounted read-only at `/etc/ansibled`) and
  `data/` (uid 10001, mounted at `/var/lib/ansibled`).
- Generates an ed25519 SSH key pair in `secrets/` if missing and sets the fact
  `ansibled_public_key` for `ansibled_target`.
- Renders `ansibled.yml` from `ansibled_config` (a dict merged over defaults).
- Writes the secrets given inline in `ansibled_config` (`git.token`,
  `git.webhook_secret`, `web.oidc.client_id`, `web.oidc.client_secret`,
  `web.metrics_token`, `web.session_key`) to files and leaves them out of the
  rendered `ansibled.yml`, generating the webhook secret, metrics token and
  session key if not given. The token and OIDC client credentials are needed on
  the first deploy only.
- Writes `ansibled_overlay_files` (list of `{path, content | src, mode}`).
- Renders `compose.yml` (host networking, `restart: always`, `:Z` volume labels
  for SELinux) and brings it up with `community.docker.docker_compose_v2`.
- Optionally registers the project webhook with `community.general.gitlab_hook`
  when `ansibled_gitlab_hook_token` is set.

### Role `ansibled_target` (every managed host)

- Local user `ansibled_target_user` (default `ansibled`), home
  `/var/lib/ansibled`, not in any directory service, so it works when
  FreeIPA/LDAP is down.
- `authorized_keys` with the daemon's key (`ansibled_target_public_key`, or read from
  `ansibled_target_controller`'s `etc/secrets/ssh_key.pub`), restricted with
  `from="<ansibled_target_from>"` when set.
- `/etc/sudoers.d/ansibled`: `NOPASSWD: ALL`, validated with `visudo -cf`.
- SELinux hosts: `ssh_home_t` context on the `.ssh` folder.
- Works on RHEL-family and Debian-family hosts.

## Repository layout

```
cmd/ansibled/            main, CLI
internal/config/         load + validate
internal/forge/          interface and types
internal/forge/gitlab/   GitLab adapter (webhooks, API)
internal/oidc/           OpenID Connect sign-in for the web UI (oidctest: fake provider)
internal/store/          SQLite
internal/queue/          queue, supersede, worker
internal/engine/         fetch, checkout, overlay, setup, ansible, parse
internal/report/         status descriptions, MR comment markdown
internal/metrics/        Prometheus collectors
internal/web/            listeners, log pages, auth
callback/                Ansible callback plugin
collection/              Ansible collection (roles ansibled, ansibled_target)
docs/                    alerts.yml, setup guides
examples/                example config
testdata/                fixture repo, webhook payloads
Containerfile
.github/workflows/
```

## Testing

- Go unit tests: config validation, webhook parsing (recorded GitLab payloads
  in `testdata/`), trust decisions, queue supersede/dedupe, summary parsing,
  comment rendering.
- Engine test against `testdata/sample-repo` (a tiny uv project plus a playbook
  that templates a file) running against an sshd container.
- End-to-end with docker: ansibled + a fake GitLab (Go httptest server
  recording API calls) + a fake OpenID Connect provider (`oidctest`) + an sshd
  target.

## Later

- "What this MR changes compared with main": diff the MR run's changed tasks
  against the latest `main` run so existing drift drops out of MR reports.
- Real applies (`mode: apply`), with reboot gating.
- GitHub adapter (commit statuses with `context`, issue comments, team membership).
