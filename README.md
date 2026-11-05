# Boxlet

**English** | [Русский](README.ru.md)

A lightweight container manager — a **free and open-source** alternative
to Docker Desktop and OrbStack for macOS.

The key principle is **maximum compatibility**: a real Docker Engine
(`dockerd`) runs inside the VM, so your existing tools (`docker` CLI,
`docker compose`, testcontainers, and any Docker API client) work
unchanged, and switching from Docker Desktop / OrbStack takes minutes.
Boxlet is free for everyone, including commercial use — no accounts,
no subscriptions, no telemetry (Apache 2.0, see [LICENSE](LICENSE)
and [NOTICE](NOTICE)).

## Features

- **Fast startup on Apple Silicon** — a Virtualization.framework VM
  boots in seconds, no third-party hypervisor
- **Real dockerd** — full API compatibility, including `docker run -it`,
  attach, and testcontainers
- **Rosetta** — `linux/amd64` images work out of the box
- **Host directory bind mounts** via virtiofs: `-v ~/projects:/src`
  just works
- **Transparent port forwarding** of container ports to `127.0.0.1`,
  including privileged ports <1024 (via `boxlet priv install`)
- **mTLS to dockerd** — the Docker API in the VM rejects connections
  without a client certificate (issued automatically on first start)
- **`*.boxlet.local` names** for published ports (`boxlet dns install`)
- **Menu bar tray** (`boxlet tray`) and a **web UI** (status, containers,
  live metrics, logs, restart)
- **Full-lifecycle CLI**: `init`, `start`, `stop`, `status`, `logs`,
  `shell`, `set` (VM resources), `service` (launch at login), `upgrade`
- **Token-authenticated guest agent** for shell access and diagnostics
- Graceful guest shutdown (sync + poweroff); the data disk survives
  restarts and upgrades

## How it works

```
┌─────────────────────────────┐
│   CLI / web UI / menu tray  │
└──────────────┬──────────────┘
               │
┌──────────────▼──────────────┐
│  boxletd (supervisor daemon)│
│  ├─ socket proxy            │◄── ~/.boxlet/run/docker.sock
│  │     (/var/run/docker.sock├── symlink for compatibility)
│  └─ backend driver          │
└──────────────┬──────────────┘
               │
┌──────────────▼──────────────┐
│  Linux VM / native runtime  │
│  └─ dockerd (Moby)          │
└─────────────────────────────┘
```

Backends (container runtime drivers):

| Platform | Driver | Status |
|---|---|---|
| macOS (Apple Silicon) | `darwin-vz` — Virtualization.framework via a Swift helper | works: Rosetta (linux/amd64), virtiofs bind mounts |
| macOS / Linux | `qemu` — diskless Alpine VM with dockerd | works |
| Windows | `wsl2` — managed WSL2 distribution | scaffold |
| Linux | `linux` — native dockerd, no VM | scaffold |

The backend is selected by the `backend` field in `~/.boxlet/config.json`
(`darwin-vz`, `qemu`). On Apple Silicon `darwin-vz` is recommended:
seconds-fast boot, Rosetta, and host directory bind mounts.

Inside the VM runs a minimal diskless Alpine Linux with `dockerd`,
so the API and behavior are identical to "real" Docker.

## Quick start

### 1. Install from a prebuilt release (macOS, Apple Silicon)

```sh
curl -fsSL https://raw.githubusercontent.com/alexandr-mironov/boxlet/main/install.sh | bash
```

The script downloads the latest release, verifies the sha256 checksum,
and places the binaries into `~/.boxlet/bin` (plus a
`/usr/local/bin/boxlet` symlink if writable). No sudo required.

### 1-alt. Build from source

```sh
# dependencies: qemu (brew install qemu) — only for the qemu backend;
# darwin-vz builds with the stock swiftc from Xcode CLT
make build   # on macOS also builds and ad-hoc signs bin/boxlet-vm
export PATH="$PWD/bin:$PATH"
```

### 2. Run

```sh
# initialize (creates ~/.boxlet, downloads Alpine images)
boxlet init

# start (the first VM boot takes a few minutes)
boxlet start

# check the environment
boxlet doctor

# then just use docker
export DOCKER_HOST=unix://$HOME/.boxlet/run/docker.sock
docker ps
```

### 3. Optional

```sh
boxlet priv install      # ports <1024 and the /var/run/docker.sock symlink
                         # (one time, macOS GUI password prompt)
boxlet dns install       # *.boxlet.local names resolve to the guest OS
boxlet service install   # launch the daemon at login
boxlet tray              # menu bar icon: status, metrics, start/stop
```

### Uninstall

```sh
boxlet stop
boxlet service uninstall  # if you enabled launch at login
boxlet dns uninstall      # if you enabled DNS
sudo rm /var/run/docker.sock  # if you created the symlink
rm -rf ~/.boxlet /usr/local/bin/boxlet
```

## Repository layout

- `cmd/boxlet` — CLI (init/start/stop/status/doctor/shell/logs/dns/tray/set/service/upgrade/ps/...)
- `cmd/boxletd` — daemon: manages the VM, socket proxy, DNS, and web UI
- `cmd/boxlet-agent` — guest OS agent for `boxlet shell` (linux, static)
- `cmd/boxlet-priv` — setuid helper: ports <1024, /var/run/docker.sock
  symlink, /etc/resolver/boxlet
- `internal/backend` — driver interface + per-platform implementations
- `internal/guest` — guest OS: Alpine downloads, apkovl overlay
- `internal/proxy` — Docker API socket proxying from the VM to the host
- `internal/dnsd` — DNS server for the *.boxlet.local zone → guest OS IP
- `internal/config`, `internal/paths` — instance configuration and paths
- `internal/webui` — embedded web UI (status, containers, live metrics,
  logs, restart)
- `vmhelper/` — the `boxlet-vm` Swift helper (Virtualization.framework)
  and entitlements for ad-hoc signing
- `tray/` — macOS menu bar tray `boxlet-tray` (AppKit, no dependencies)
- `install.sh` — release installer (curl | bash)
- `.github/workflows/` — CI (vet, tests, build) and the tag-driven
  release pipeline
- `docs/` — architecture, roadmap, compatibility notes

## Documentation

- [Architecture](docs/architecture.md)
- [Compatibility](docs/compat.md)
- [Roadmap](docs/roadmap.md)

Note: in-depth docs are currently in Russian; an English translation
is on the way.

## License

[Apache License 2.0](LICENSE) — free and open source, commercial use
allowed. Derivative works must retain the attribution notice from
[NOTICE](NOTICE) with a link to this repository (§4(d)).
