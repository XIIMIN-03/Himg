FROM golang:1.22-bookworm AS builder

WORKDIR /src

COPY code/go.mod code/go.sum ./
RUN go mod download

COPY code/ .

RUN apt-get update \
    && apt-get install -y --no-install-recommends gcc libc6-dev libwebp-dev \
    && rm -rf /var/lib/apt/lists/*

RUN CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -o /out/himg .

FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates libsqlite3-0 libwebp7 \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /data

ENV HIMG_DATA_DIR=/data \
    HIMG_DB_FILE=/data/himg.db \
    HIMG_THEMES_DIR=/themes \
    LOCAL_UPLOAD_DIR=/data/uploads

COPY --from=builder /out/himg /usr/local/bin/himg
COPY data/ban.webp /opt/himg-defaults/ban.webp
COPY themes/default /opt/himg-defaults/themes/default
COPY docker/docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh

RUN chmod +x /usr/local/bin/docker-entrypoint.sh \
    && mkdir -p /data/uploads /themes

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
CMD ["himg"]
