FROM golang:1.26.6-alpine@sha256:3889b425f035be855a72fb4755265311293b6d414521f0a519d819df32222d83 AS build

ARG SERVICE=api
ARG VERSION=dev
ARG REVISION=unknown
ARG TARGETOS=linux
ARG TARGETARCH
ARG GOPROXY=https://proxy.golang.org,direct

WORKDIR /workspace
COPY go.mod go.sum ./
RUN GOPROXY="${GOPROXY}" go mod download
COPY src/backend ./src/backend
RUN case "${SERVICE}" in api|worker|migrator|identity-bootstrap) ;; *) exit 2 ;; esac && \
    CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
    go build -trimpath \
      -ldflags="-s -w -X github.com/snowflake-yezi/mail-suite/src/backend/internal/platform/buildinfo.Version=${VERSION} -X github.com/snowflake-yezi/mail-suite/src/backend/internal/platform/buildinfo.Commit=${REVISION}" \
      -o /out/mail-suite "./src/backend/cmd/${SERVICE}"

FROM scratch

ARG VERSION=dev
ARG REVISION=unknown
LABEL org.opencontainers.image.title="mail-suite backend" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.source="https://github.com/snowflake-yezi/mail-suite"

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/mail-suite /mail-suite

USER 65532:65532
EXPOSE 8080 8081
ENTRYPOINT ["/mail-suite"]
