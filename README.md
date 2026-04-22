# ZFS Dashboard

A lightweight web dashboard for monitoring ZFS pools, datasets, snapshots, drives and ARC cache — packaged as a single Go binary and a Docker image.

## Features

- **Pools** — health, capacity, fragmentation, dedup ratio, I/O ops & bandwidth, vdev tree
- **Datasets** — usage, available space, compression ratio
- **Snapshots** — creation date, referenced/used size, per-dataset grouping
- **Drives** — SMART health, temperature, power-on hours, per-attribute warnings
- **ARC** — hit rate, MFU/MRU sizes, demand vs prefetch breakdown
- Dark / light theme, responsive layout

## Quick start

```bash
docker run -d \
  --name zfs-dashboard \
  --privileged \
  -p 8080:8080 \
  -v /usr/local/sbin/zpool:/usr/local/sbin/zpool:ro \
  -v /usr/local/sbin/zfs:/usr/local/sbin/zfs:ro \
  -v /lib64:/hostlib64:ro \
  -v /dev/disk:/dev/disk:ro \
  -v /proc/spl:/proc/spl:ro \
  ghcr.io/qjoly/zfs-dashboard:latest
```

Then open [http://localhost:8080](http://localhost:8080).

## Configuration

All options are set via environment variables:

| Variable       | Default                              | Description                              |
|----------------|--------------------------------------|------------------------------------------|
| `LISTEN`       | `:8080`                              | Address and port to listen on            |
| `ZPOOL_CMD`    | `/usr/local/sbin/zpool`              | Path to the `zpool` binary               |
| `ZFS_CMD`      | `/usr/local/sbin/zfs`                | Path to the `zfs` binary                 |
| `SMART_CMD`    | `/usr/sbin/smartctl`                 | Path to the `smartctl` binary            |
| `DISK_BY_ID`   | `/dev/disk/by-id`                    | Path to disk-by-id symlinks              |
| `HOST_LD`      | *(unset)*                            | Host dynamic linker path (see below)     |
| `HOST_LIBPATH` | `/hostlib64:/usr/local/lib`          | Library search path used with `HOST_LD` |

### Host linker workaround

The container ships AlmaLinux 9 libraries. If the host ZFS binaries were compiled against a different glibc (common on Debian/Ubuntu), set `HOST_LD` to the host dynamic linker and bind-mount the host `/lib64`:

```bash
-e HOST_LD=/hostlib64/ld-linux-x86-64.so.2 \
-v /lib64:/hostlib64:ro
```

## Build

### Local

```bash
go build -ldflags="-s -w" -o zfs-dashboard .
```

### Docker

```bash
docker build -t zfs-dashboard .
```

## Docker image

Images are published to the GitHub Container Registry on every push to `main` and on version tags:

```
ghcr.io/qjoly/zfs-dashboard:latest
ghcr.io/qjoly/zfs-dashboard:v1.0.0
```

## API

The dashboard exposes a single JSON endpoint used by the frontend:

```
GET /api/data
```

Returns pools, datasets, snapshots, drives and ARC stats in a single payload.
