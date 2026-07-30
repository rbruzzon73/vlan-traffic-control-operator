FROM golang:1.22 as builder
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -o manager cmd/manager/main.go

FROM registry.access.redhat.com/ubi9/ubi-minimal:latest
WORKDIR /
COPY --from=builder /workspace/manager /manager
USER 65532:65532
ENTRYPOINT ["/manager"]
