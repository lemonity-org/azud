# Stage 1: Build static binary
FROM docker.io/library/golang:1.26.5-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2 AS builder

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
