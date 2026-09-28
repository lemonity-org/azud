# Stage 1: Build static binary
FROM docker.io/library/golang:1.26.6-alpine@sha256:3889b425f035be855a72fb4755265311293b6d414521f0a519d819df32222d83 AS builder

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

RUN CGO_ENABLED=0 go build \
    -ldflags "-X github.com/lemonity-org/azud/pkg/version.Version=${VERSION} \
              -X github.com/lemonity-org/azud/pkg/version.Commit=${COMMIT} \
              -X github.com/lemonity-org/azud/pkg/version.BuildDate=${BUILD_DATE}" \
    -o /azud ./cmd/azud

# Stage 2: Minimal runtime image
FROM docker.io/library/alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b

RUN apk add --no-cache openssh-client ca-certificates curl \
    && adduser -D -h /home/azud azud

USER azud
WORKDIR /home/azud

COPY --from=builder /azud /usr/local/bin/azud

ENTRYPOINT ["azud"]
