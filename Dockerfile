# ── Stage 1: Build ────────────────────────────────────────────────────────────
FROM golang:1.22-bullseye AS builder

WORKDIR /app
COPY go.mod .
COPY main.go .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o zfs-dashboard .

# ── Stage 2: Runtime ──────────────────────────────────────────────────────────
FROM almalinux:10

# smartmontools is used for SMART data collection
RUN dnf install -y epel-release && \
    dnf install -y smartmontools && \
    dnf clean all && \
    rm -rf /var/cache/dnf

COPY --from=builder /app/zfs-dashboard /usr/local/bin/zfs-dashboard

EXPOSE 8080

# ZFS binaries + their /lib64 libs are bind-mounted from the host at runtime.
# HOST_LD tells the app to invoke host binaries via the host dynamic linker,
# bypassing glibc version mismatches between the container and the host.
ENV ZPOOL_CMD=/usr/local/sbin/zpool \
    ZFS_CMD=/usr/local/sbin/zfs \
    SMART_CMD=/usr/sbin/smartctl \
    DISK_BY_ID=/dev/disk/by-id \
    LISTEN=:8080 \
    HOST_LD=/hostlib64/ld-linux-x86-64.so.2 \
    HOST_LIBPATH=/hostlib64:/usr/local/lib

CMD ["/usr/local/bin/zfs-dashboard"]
