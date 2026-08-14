# Offline/no-pull image.
#
# Build binaries first:
#   PowerShell: ./scripts/build-dist.ps1
#   Bash:       ./scripts/build-dist.sh
#
# Then:
#   docker compose build
#
# This Dockerfile intentionally uses FROM scratch so Docker does not pull
# golang/alpine from Docker Hub or any mirror.
FROM scratch

COPY dist/linux-amd64/cfdata /usr/local/bin/cfdata
COPY dist/linux-amd64/cfnat /usr/local/bin/cfnat
COPY dist/linux-amd64/cloudflare-web /usr/local/bin/cloudflare-web
COPY dist/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

ENV SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt \
    DATA_DIR=/data \
    WEB_ADDR=0.0.0.0:8080 \
    HEALTHCHECK_URL=http://127.0.0.1:8080/api/health \
    CFNAT_AUTO_START=true \
    CFNAT_ADDR=0.0.0.0:1234 \
    CFNAT_CODE=200 \
    CFNAT_COLO="" \
    CFNAT_DELAY=300 \
    CFNAT_DOMAIN=cloudflaremirrors.com/debian \
    CFNAT_FIXED_IPS="" \
    CFNAT_IPNUM=20 \
    CFNAT_IPS=4 \
    CFNAT_NUM=5 \
    CFNAT_PORT=443 \
    CFNAT_RANDOM=true \
    CFNAT_TASK=100 \
    CFNAT_TLS=true \
    PROXY_AUTO_APPLY=false \
    PROXY_AUTO_HOST="" \
    PROXY_AUTO_PATH=/ \
    PROXY_AUTO_PORT=443 \
    PROXY_AUTO_CONCURRENCY=20 \
    PROXY_AUTO_MAX_LATENCY=5000 \
    PROXY_AUTO_POOL_SIZE=5 \
    PROXY_AUTO_MIN_POOL=3 \
    PROXY_AUTO_CFDATA=false \
    PROXY_AUTO_CFDATA_TIMEOUT=600 \
    PROXY_CFDATA_CANDIDATES=300 \
    PROXY_OFFICIAL_CANDIDATES=150

WORKDIR /data
EXPOSE 8080 1234
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD ["/usr/local/bin/cloudflare-web", "healthcheck"]

ENTRYPOINT ["/usr/local/bin/cloudflare-web"]
