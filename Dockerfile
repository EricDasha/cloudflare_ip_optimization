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
COPY dist/linux-amd64/sing-box /usr/local/bin/sing-box
COPY dist/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY third_party/sing-box/LICENSE /usr/share/licenses/sing-box/LICENSE

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
    CFNAT_FALLBACK="" \
    CFNAT_NUM=1 \
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
    PROXY_DOMAIN_FORWARD="" \
    PROXY_AUTO_DOMAINS=false \
    PROXY_PREFERRED_MAX_DOMAINS=20 \
    PROXY_PREFERRED_RESOLVE=true \
    PROXY_PREFERRED_IPS_PER_DOMAIN=4 \
    PROXY_PREFERRED_RESOLVED_CANDIDATES=64 \
    PROXY_CFDATA_CANDIDATES=300 \
    PROXY_OFFICIAL_CANDIDATES=150 \
    PROXY_USER_CANDIDATES="" \
    PROXY_SUBSCRIPTION_CONVERTER=https://vlesdy.trojanjd.dpdns.org/sub \
    PROXY_SUBSCRIPTION_MAINTAINERS=owo.o00o.ooo,cm.soso.edu.kg,zrf.zrf.me,sub.keaeye.icu,sub.mot.cloudns.biz,sub.mia.xx.kg,sub.lzjbaby.com,sub.xdu.qzz.io \
    PROXY_SUBSCRIPTION_TOKEN="" \
    PROXY_SUBSCRIPTION_REFRESH_ENABLED=true \
    PROXY_SUBSCRIPTION_REFRESH_MINUTES=360 \
    PROXY_SUBSCRIPTION_PER_MAINTAINER=30 \
    PROXY_SUBSCRIPTION_FETCH_TIMEOUT=45 \
    PROXY_SUBSCRIPTION_VERIFY_TIMEOUT=240 \
    PROXY_POOL_SWITCH_COOLDOWN_MINUTES=30 \
    PROXY_ACTIVE_HEALTH_WINDOW_MINUTES=60 \
    PROXY_VLESS_PROBE=false \
    PROXY_VLESS_TEMPLATE=/data/vless-probe-outbound.json \
    PROXY_VLESS_TEST_URL=https://www.gstatic.com/generate_204 \
    PROXY_VLESS_EXPECT_STATUS=204 \
    PROXY_VLESS_TIMEOUT=15 \
    PROXY_VLESS_MAX_CANDIDATES=20 \
    PROXY_VLESS_SPEED_BYTES=2097152 \
    PROXY_VLESS_SPEED_TIMEOUT=20 \
    PROXY_VLESS_MIN_DOWNLOAD_MBPS=3 \
    PROXY_BACKGROUND_OPTIMIZER=true \
    PROXY_SCHEDULER_PROBE_INTERVAL_SECONDS=300 \
    PROXY_SCHEDULER_BATCH_SIZE=12 \
    PROXY_SCHEDULER_APPLY=true \
    SING_BOX_BIN=/usr/local/bin/sing-box

WORKDIR /data
EXPOSE 8080 1234
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD ["/usr/local/bin/cloudflare-web", "healthcheck"]

ENTRYPOINT ["/usr/local/bin/cloudflare-web"]
