# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/borg-monitor ./cmd/borg-monitor

FROM alpine:3
# borg (1.4.x from Alpine), ssh for remote repositories, prlimit (util-linux-misc)
# for the memory limit of restore tests, tzdata for borg 1.x local timestamps.
RUN apk add --no-cache borgbackup openssh-client util-linux-misc tzdata ca-certificates \
 && addgroup -g 1000 borgmon && adduser -D -u 1000 -G borgmon -h /data borgmon \
 && mkdir -p /config /secrets /data && chown borgmon:borgmon /data
COPY --from=build /out/borg-monitor /usr/local/bin/borg-monitor
USER borgmon
ENV BBM_CONFIG=/config/config.yaml BBM_LISTEN=0.0.0.0:8080
EXPOSE 8080
VOLUME ["/data"]
HEALTHCHECK --interval=60s --timeout=5s CMD wget -q -O /dev/null http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["borg-monitor"]
