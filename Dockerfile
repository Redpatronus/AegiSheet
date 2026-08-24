# syntax=docker/dockerfile:1
#
# AegiSheet container image.
#
# Two stages: a full Go toolchain to compile, and a distroless runtime that holds
# nothing but the binary, the CA certificates needed to reach Google, and a
# writable directory for the generated workbook. The result has no shell, no
# package manager and no libc, which removes most of what an attacker could use if
# they ever reached the container — and it runs as an unprivileged user.
#
# Build:
#   docker build -t aegisheet:dev .
#
# Run (mount the configuration and credentials read-only, the output directory
# read-write):
#   docker run --rm \
#     -v "$PWD/conf/mine.ini:/etc/aegisheet/report.ini:ro" \
#     -v "$PWD/sa.json:/etc/aegisheet/sa.json:ro" \
#     -v "$PWD/out:/data" \
#     aegisheet:dev \
#       -config /etc/aegisheet/report.ini \
#       -service-account /etc/aegisheet/sa.json \
#       -impersonate admin@example.com \
#       -output /data/report.xlsx \
#       -log-format json -non-interactive

# ---------------------------------------------------------------------------
# Build stage
# ---------------------------------------------------------------------------
# Pinned to a minor release so that patch updates — which is how standard library
# advisories are fixed — arrive with a rebuild, while the language version stays
# predictable. This is deliberately ahead of the floor declared in go.mod: the
# floor is the oldest toolchain that is safe to build with, not the one a release
# artefact should be built with.
FROM golang:1.27-alpine AS build

# Build metadata, injected at link time so the running container can identify
# exactly which commit produced it.
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

WORKDIR /src

# Dependencies are copied and downloaded on their own so that the layer is reused
# whenever only application code changes.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download && go mod verify

COPY cmd/ ./cmd/
COPY internal/ ./internal/

# CGO_ENABLED=0 produces a static binary, which is what lets the runtime stage be
# distroless/static. -trimpath keeps build paths out of the binary, and
# -buildvcs=false is required because .dockerignore excludes .git from the build
# context.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -buildvcs=false \
      -ldflags "-s -w \
        -X github.com/Redpatronus/AegiSheet/internal/buildinfo.version=${VERSION} \
        -X github.com/Redpatronus/AegiSheet/internal/buildinfo.commit=${COMMIT} \
        -X github.com/Redpatronus/AegiSheet/internal/buildinfo.date=${BUILD_DATE}" \
      -o /out/aegisheet ./cmd/aegisheet

# Fail the build rather than ship a binary that cannot start.
RUN /out/aegisheet -version

# Prepare the working directory here: the runtime image has no shell to mkdir
# with, and the ownership has to match the unprivileged runtime user.
RUN mkdir -p /out/data && chown -R 65532:65532 /out/data

# ---------------------------------------------------------------------------
# Runtime stage
# ---------------------------------------------------------------------------
# distroless/static provides CA certificates, /etc/passwd for the nonroot user and
# time zone data — nothing else. :nonroot selects uid/gid 65532.
FROM gcr.io/distroless/static-debian12:nonroot

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

# Standard OCI annotations, so a registry and a running container can be traced
# back to their source.
LABEL org.opencontainers.image.title="AegiSheet" \
      org.opencontainers.image.description="Google Workspace administrative reporting to Excel" \
      org.opencontainers.image.source="https://github.com/Redpatronus/AegiSheet" \
      org.opencontainers.image.documentation="https://github.com/Redpatronus/AegiSheet#readme" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${BUILD_DATE}" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.base.name="gcr.io/distroless/static-debian12:nonroot"

COPY --from=build /out/aegisheet /usr/local/bin/aegisheet

# The example configuration ships as documentation. It is deliberately not the
# default: a real run mounts its own file, because this one contains only
# placeholders.
COPY conf/xls_report.ini /usr/share/aegisheet/xls_report.ini

# A writable location for generated workbooks, owned by the runtime user. Mount a
# volume over it to keep the report after the container exits.
COPY --from=build --chown=65532:65532 /out/data /data

# uid:gid rather than a name, so the image works unchanged under a Kubernetes
# runAsNonRoot policy that verifies the numeric user.
USER 65532:65532
WORKDIR /data

ENTRYPOINT ["/usr/local/bin/aegisheet"]
# Without arguments the container explains itself instead of failing on a missing
# configuration file.
CMD ["-h"]
