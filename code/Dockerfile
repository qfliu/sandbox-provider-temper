# Platform image with both binaries, /api (control plane) and /gateway (SSH
# entry point). Built inside minikube by code/scripts/deploy.sh:
#   minikube image build -t sandbox/api:<tag> code
#
# For another architecture (amd64 nodes, D63): add
#   --build-opt=opt=platform=linux/amd64
# Stage 1 runs on the build machine's own architecture and cross-compiles
# (Go needs no emulation for that), so only stage 2's copy is foreign.
#
# Stage 1 compiles a static binary; stage 2 is distroless: no shell, no
# package manager, runs as a non-root user, so there's very little to attack.

FROM --platform=$BUILDPLATFORM golang:1.27.1 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
# Dependencies first, so code changes don't re-download modules.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/api ./cmd/gateway

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/api /out/gateway /
USER nonroot:nonroot
EXPOSE 8080 2222
ENTRYPOINT ["/api"]
