# Cross-compiles on the build machine's architecture (no emulation needed
# for multi-arch builds) and ships a static binary on distroless.
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags="-s -w -X github.com/rothgar/nodenap/internal/scaler.Version=${VERSION}" \
    -o /nodenap ./cmd/nodenap

FROM gcr.io/distroless/static:nonroot
COPY --from=build /nodenap /nodenap
USER 65532:65532
ENTRYPOINT ["/nodenap"]
