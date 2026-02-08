# Stage 1: Build static binary
FROM golang:1.25-alpine AS builder

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
FROM alpine:3.21

RUN apk add --no-cache openssh-client ca-certificates curl \
    && adduser -D -h /home/azud azud

USER azud
WORKDIR /home/azud

COPY --from=builder /azud /usr/local/bin/azud

ENTRYPOINT ["azud"]
