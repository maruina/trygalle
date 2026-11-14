# Stage 1: build the static Trygalle binary. Cross-compiling on the build
# platform keeps the arm64 build off QEMU; CGO is not in the build path.
FROM --platform=$BUILDPLATFORM golang:1.27-bookworm AS build
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOARCH=$TARGETARCH go build -trimpath -o /out/trygalle ./cmd/trygalle

# Stage 2: runtime.
FROM node:24-bookworm-slim
ARG TARGETARCH
# Pinned to harness.PinnedPiVersion; Renovate bumps every pin surface in one PR.
RUN npm install -g --ignore-scripts @earendil-works/pi-coding-agent@1.5.2
# kubectl is not in the Debian archive; install the pinned release binary and
# verify it against its published SHA256 checksum. Kubectl is deliberately not
# Renovate-managed; bump it to stay within one minor of the target API server.
ARG KUBECTL_VERSION=v1.37.2
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
       bash git curl jq ripgrep ca-certificates \
    && rm -rf /var/lib/apt/lists/*
RUN curl -fsSL -o /usr/local/bin/kubectl \
      https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl \
    && echo "$(curl -fsSL https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl.sha256)  /usr/local/bin/kubectl" \
       | sha256sum --check \
    && chmod 0755 /usr/local/bin/kubectl
COPY --from=build /out/trygalle /usr/local/bin/trygalle
COPY trygalle-entrypoint /usr/local/bin/trygalle-entrypoint
# Pi's working directory and Trygalle's session directory are the two paths
# the process must be able to write; both live on the PVC in the chart, and
# the image creates them so a plain `docker run` with a bind mount also works.
RUN mkdir -p /data/pi/sessions /data/pi/work \
    && chown -R node:node /data/pi
LABEL org.opencontainers.image.source=https://github.com/maruina/trygalle
USER node
WORKDIR /data/pi/work
ENTRYPOINT ["/usr/local/bin/trygalle-entrypoint"]
