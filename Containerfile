# syntax=docker/dockerfile:1

# ---- build: static Go binary, cross-compiled on the build platform ----
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/ansibled ./cmd/ansibled

# ---- uv, pinned ----
FROM ghcr.io/astral-sh/uv:0.9.9 AS uv

# ---- runtime: Python for Ansible, plus git/ssh/tini ----
FROM python:3.13-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends git openssh-client tini ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --uid 10001 --user-group --home-dir /var/lib/ansibled/home --no-create-home ansibled \
 && mkdir -p /etc/ansibled /var/lib/ansibled \
 && chown ansibled:ansibled /var/lib/ansibled

COPY --from=uv /uv /uvx /usr/local/bin/
COPY --from=build /out/ansibled /usr/local/bin/ansibled
COPY callback/ /usr/share/ansibled/callback_plugins/

ENV UV_PYTHON_DOWNLOADS=automatic \
    UV_LINK_MODE=copy
USER ansibled
WORKDIR /var/lib/ansibled
VOLUME ["/var/lib/ansibled"]
ENTRYPOINT ["tini", "--", "ansibled"]
CMD ["serve"]
