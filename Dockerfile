# Stage 1: Build static binary
FROM docker.io/library/golang:1.27.4-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 AS builder

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
FROM docker.io/library/alpine:3.24@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a

RUN apk add --no-cache openssh-client ca-certificates curl \
    && adduser -D -h /home/azud azud

USER azud
WORKDIR /home/azud

COPY --from=builder /azud /usr/local/bin/azud

ENTRYPOINT ["azud"]
