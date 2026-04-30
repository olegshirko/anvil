<!--
  Anvil README
-->

<h1 align="center">Anvil</h1>
<p align="center">
  <b>Forge container runtimes on macOS and Linux — while it's hot.</b>
</p>

<p align="center">
  <a href="https://github.com/olegshirko/anvil/actions/workflows/go.yml"><img src="https://img.shields.io/github/actions/workflow/status/olegshirko/anvil/go.yml?branch=main&label=build&style=flat-square" alt="Build"></a>
  <a href="https://github.com/olegshirko/anvil/releases/latest"><img src="https://img.shields.io/github/v/release/olegshirko/anvil?sort=semver&style=flat-square&color=orange" alt="Release"></a>
  <a href="https://golang.org/doc/go1.25"><img src="https://img.shields.io/badge/go-1.25+-00ADD8?style=flat-square&logo=go" alt="Go"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-blue?style=flat-square" alt="License"></a>
  <a href="https://brew.sh"><img src="https://img.shields.io/badge/brew-olegshirko%2Ftap-FFBB00?style=flat-square&logo=homebrew" alt="Homebrew"></a>
</p>

---

## At the Forge

Docker Desktop is like a sledgehammer: heavy, unwieldy, and merciless on your battery.  
**Anvil** is like a developer's anvil: built to be convenient, and your Air won't melt (though it's still a bit heavy for now).

- **Clean Architecture** — longitivity infrastructure CLI tools.
- **Tempered boot times** — cold start in ~20 s.
- **Alloy of runtimes** — Docker, Containerd, or Incus.
- **Mirror fallback** — when Docker Hub is unreachable, Anvil pulls from GitHub.

---

## Installation

```sh
# 1. Install
brew tap olegshirko/tap
brew install anvil

# 2. Start
anvil start

# 3. Shape metal
docker run hello-world
```

---

## Features

|  | Feature |
|---|---|
| 🐳 | **Docker** — full Docker CE inside a Lima VM, exposed natively on macOS |
| 🦭 | **Containerd** — lightweight alternative with `nerdctl` and Buildkit |
| 🧊 | **Incus** — system containers and virtual machines (VM mode on Apple Silicon M3+) |
| ☸️ | **Kubernetes** — optional K3s inside the VM, zero host clutter |
| 🍎 | **Apple Silicon & Intel** — native `vz` on macOS 13+, QEMU fallback |
| 🐧 | **Linux** — Linux, no comments |
| 🪞 | **Docker Mirror** — fallback for air-gapped environments when Docker Hub is unreachable |
| ⚡ | **Parallel Downloads** — multi-threaded HTTP Range downloads for large images |
| 🏗️ | **Clean Architecture** — ... |
| 🔧 | **Multi-instance** — profiles let you run Docker *and* Containerd side by side |
| 📦 | **Multi-version images** — Ubuntu 24.04, 26.04, and beyond |

---

### Docker *(default)*

```sh
anvil start
```

The `docker` client on macOS works straight away.

### Containerd

```sh
anvil start --runtime containerd
anvil nerdctl install   # optional: put nerdctl in your $PATH
```

### Kubernetes

```sh
anvil start --kubernetes
```

K3s spins up inside the VM. Images built or pulled with Docker are automatically accessible to Kubernetes.

### Incus

```sh
anvil start --runtime incus
```

System containers and virtual machines. VM mode requires Apple Silicon M3 or newer.

### No Runtime

```sh
anvil start --runtime none
```

Just a Lima VM — no container runtime. Sometimes that's all you need.

---

## Mirror

When the Docker registry is down, Anvil doesn't give up.

```sh
# Discover what's available
anvil images list

# Check before you pull
anvil images check --docker postgres:15.8

# Load from mirror directly
anvil images load --docker postgres:15.8
```

`anvil compose up -d` automatically falls back to the mirror when `docker pull` fails — no manual intervention.

If all else fails, request missing images via GitHub issue:

```sh
anvil images request --docker postgres:15.8
```

---

### Defaults

Default VM: 2 CPUs, 2 GiB memory, 100 GiB storage.

```sh
# Smaller hammer
anvil start --cpu 1 --memory 2 --disk 10

# Bigger hammer
anvil stop
anvil start --cpu 4 --memory 8

# Rosetta 2 alloy (macOS 13+ Apple Silicon)
anvil start --vm-type=vz --vz-rosetta
```

Edit config from the CLI:

```sh
anvil start --edit
```

### Diagnostics

```sh
anvil doctor
```

### Status Checks

```sh
anvil status
anvil health
```

---

## Installation

| Method | Command |
|---|---|
| **Homebrew** (recommended) | `brew tap olegshirko/tap && brew install anvil` |
| **Homebrew HEAD** | `brew install --HEAD anvil` |
| **Nix** | `nix-build` |
| **Binary** | See [Releases](https://github.com/olegshirko/anvil/releases) |
| **Source** | `git clone … && make && sudo make install` |

**Requirements:** macOS (Intel or Apple Silicon) or Linux, plus `limactl` and QEMU (or `vz` on macOS 13+).

---

## License

MIT — see [LICENSE](LICENSE).

---

<p align="center">
  <i>Every great tool starts at the anvil.</i>
</p>
