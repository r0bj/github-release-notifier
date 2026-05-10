FROM golang:1.26.3 AS builder

WORKDIR /workspace

COPY go.mod go.sum ./
RUN go mod download

COPY *.go ./

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o github-release-notifier .

FROM alpine:3.23 AS certs

RUN apk add --no-cache ca-certificates


FROM scratch

COPY --from=builder /workspace/github-release-notifier /
COPY --from=certs /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

USER 65534:65534

ENTRYPOINT ["/github-release-notifier"]
