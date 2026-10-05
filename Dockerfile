# Armageddon server image for the Docker Compose deployment (contract §9.2,
# plan M5.7): the same single binary plus git, with /var/lib/armageddon on a
# volume. See deploy/compose.yaml.
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=unknown
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" -o /out/armageddon ./cmd/armageddon

FROM debian:bookworm-slim
# git >= 2.40 (contract §9.1): Debian 12 ships 2.39, backports has a newer one.
RUN echo 'deb http://deb.debian.org/debian bookworm-backports main' >/etc/apt/sources.list.d/backports.list \
 && apt-get update \
 && apt-get install -y --no-install-recommends -t bookworm-backports git \
 && apt-get install -y --no-install-recommends ca-certificates curl tini passwd util-linux procps \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --system --user-group --home-dir /var/lib/armageddon --no-create-home --shell /usr/sbin/nologin armageddon
COPY --from=build /out/armageddon /usr/local/bin/armageddon
COPY deploy/docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
VOLUME /var/lib/armageddon
EXPOSE 8080 443 80
HEALTHCHECK --interval=15s --timeout=10s --start-period=20s CMD ["armageddon", "server", "health", "--data", "/var/lib/armageddon", "--timeout", "5s"]
ENTRYPOINT ["tini", "--", "docker-entrypoint.sh"]
