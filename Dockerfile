# syntax=docker/dockerfile:1.7
ARG GO_IMAGE=golang:1.26.6-bookworm@sha256:116d58cbd88c1297624acc6e967a060012422bacf9930927e23fb719189c6f36
ARG RUNTIME_IMAGE=debian:bookworm-slim@sha256:abd67ffcfa541b485a3dff59865ab629aa048a6c613e639d36e7456b0b229241

FROM ${GO_IMAGE} AS build
ARG TARGET_PACKAGE
ARG BINARY=syncbase
WORKDIR /src

COPY go.mod go.sum ./
# syncbase-embedding is a private GitHub module; the token is only mounted for
# this instruction and never written to a persisted layer.
RUN --mount=type=secret,id=github_token,required=true \
    --mount=type=cache,target=/go/pkg/mod \
    git config --global url."https://x-access-token:$(cat /run/secrets/github_token)@github.com/".insteadOf "https://github.com/" \
    && go env -w GOPRIVATE=github.com/Merge42-SyncBase/* \
    && go mod download all \
    && git config --global --unset-all url."https://x-access-token:$(cat /run/secrets/github_token)@github.com/".insteadOf

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    test -n "${TARGET_PACKAGE}" \
    && CGO_ENABLED=1 go build -trimpath -ldflags='-s -w' -o "/out/${BINARY}" "${TARGET_PACKAGE}"

FROM ${RUNTIME_IMAGE}
ARG BINARY=syncbase
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates libgomp1 util-linux \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 10001 syncbase \
    && useradd --uid 10001 --gid 10001 --no-create-home --shell /usr/sbin/nologin syncbase \
    && mkdir -p /app /data/originals \
    && chown -R syncbase:syncbase /app /data
COPY --from=build "/out/${BINARY}" /app/syncbase
COPY --chmod=0755 docker/api-entrypoint.sh /usr/local/bin/syncbase-api-entrypoint
USER 10001:10001
WORKDIR /app
ENTRYPOINT ["/app/syncbase"]
