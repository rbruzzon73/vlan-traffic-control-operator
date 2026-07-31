# Build stage
FROM golang:1.22 AS builder
WORKDIR /workspace

# Copy Go module specs
COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

# Copy source code
COPY api/ api/
COPY cmd/ cmd/
COPY pkg/ pkg/

# Build both binaries
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -o manager cmd/manager/main.go
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -o agent cmd/agent/main.go

# Runtime stage
FROM registry.access.redhat.com/ubi9/ubi-minimal:latest
WORKDIR /

# Copy tc, iproute2 tools and required libraries for host netns manipulation
RUN microdnf install -y iproute && microdnf clean all

# Copy both binaries from builder
COPY --from=builder /workspace/manager /manager
COPY --from=builder /workspace/agent /agent

USER 0
ENTRYPOINT ["/manager"]
