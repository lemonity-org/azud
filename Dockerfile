# Stage 1: Build static binary
FROM docker.io/library/golang:1.27.1-alpine@sha256:8a5910f31396cd4d89662f56c68b3ae31d374308270a1c3bd96672ee5ed43414 AS builder

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
FROM docker.io/library/alpine:3.23@sha256:fd791d74b68913cbb027c6546007b3f0d3bc45125f797758156952bc2d6daf40

RUN apk add --no-cache openssh-client ca-certificates curl \
    && adduser -D -h /home/azud azud

USER azud
WORKDIR /home/azud

COPY --from=builder /azud /usr/local/bin/azud

ENTRYPOINT ["azud"]
