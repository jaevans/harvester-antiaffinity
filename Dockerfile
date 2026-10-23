FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS builder

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev

WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o harvester-antiaffinity ./cmd/harvester-antiaffinity

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /workspace/harvester-antiaffinity /harvester-antiaffinity
USER 65532:65532
ENTRYPOINT ["/harvester-antiaffinity"]
