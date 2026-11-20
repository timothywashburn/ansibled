# timothywashburn.ansibled

Roles for deploying [ansibled](https://github.com/timothywashburn/ansibled):

- [`ansibled`](roles/ansibled/README.md): runs the daemon container on one host
  (config, secrets, SSH key, overlay, Docker Compose, optional GitLab webhook).
  Opening its ports in the host firewall is left to you.
- [`ansibled_target`](roles/ansibled_target/README.md): prepares every managed host
  (local user, SSH key with `from=`, passwordless sudo).

Requires ansible-core 2.15 or newer. Both roles validate their input
(`meta/argument_specs.yml`) and support check mode, since ansibled dry-runs the
repos that use them.

## Install

```yaml
# requirements.yml
collections:
  - name: https://github.com/timothywashburn/ansibled.git#/collection
    type: git
    version: main
```

```sh
ansible-galaxy collection install -r requirements.yml
```

This also installs the dependencies `community.docker`, `community.general`,
`community.crypto` and `ansible.posix`.

## Example

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
          listen: {webhook: "10.0.0.1:8470", web: "10.0.0.1:8471"}
          forge: {url: https://gitlab.example.org, project: myorg/infra, trusted_group: myorg/sysadmins}
          repo: {setup: uv sync --frozen --no-dev, venv_key_files: [pyproject.toml, uv.lock]}
          ansible: {inventory: inventories/mysite, playbooks: [site.yml], remote_user: ansibled}
          # Web UI sign-in: any OpenID Connect provider; access by its groups claim.
          web: {oidc: {issuer: "https://auth.example.org/application/o/ansibled/", allowed_groups: [sysadmins]}}
        # Secrets (forge token, OIDC client id/secret) on the first deploy only, from a gitignored file:
        #   ansible-playbook site.yml -e @ansibled-secrets.yml

- name: Hosts ansibled manages
  hosts: all
  become: true
  roles:
    - role: timothywashburn.ansibled.ansibled_target
      vars:
        ansibled_target_public_key: "{{ hostvars[groups['ansibled'][0]].ansibled_public_key }}"
        ansibled_target_from: [10.0.0.1]
```

See the role READMEs for every variable, and why the secrets must not be
committed to the repo ansibled checks.

## Tests

`tests/run.sh` runs both roles for real against Docker containers (Rocky Linux 9
and Debian trixie targets, a docker:dind daemon host running the image built from
the Containerfile, and a fake GitLab API for webhook registration). See the
comment at the top of the script.
