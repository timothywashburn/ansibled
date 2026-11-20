# ansibled

An Ansible dry-run daemon. ansibled watches one git repository and runs
`ansible-playbook --check --diff` against one site:

- on merge requests pushed by (or requested by) a trusted group, reporting a
  commit status and a summary comment on the MR;
- on every new commit to the default branch, reporting a commit status;
- nightly, exporting drift as Prometheus metrics.

Full logs are served by ansibled itself, behind "Sign in with GitLab".

> Status: early development. GitLab only for now; GitHub is planned.

- How it works and every config key: [DESIGN.md](DESIGN.md)
- Example config: [examples/ansibled.yml](examples/ansibled.yml)
- Ansible roles to deploy it: [collection/](collection/)

## Image

```
ghcr.io/timothywashburn/ansibled:latest
```

## Development

```sh
go test ./...
docker build -f Containerfile -t ansibled:dev .
```
