.PHONY: all build sign clean test \
    start stop daemon stop-daemon \
    service-install service-uninstall service-start service-stop service-restart service-status \
    service-debug service-debug-rebuild rebuild-all \
    docker-context-anvil docker-context-lima lima-restart colima-start colima-stop \
    boot boot-alpine boot-ubuntu boot-containerd \
    download-alpine extract-alpine-kernel \
    download-ubuntu ubuntu-modules \
    guest-agent initramfs-agent initramfs-ubuntu initramfs-containerd \
    container-tools time-boot time-service validate \
    bench bench-prepull harness harness-all prune clean-containers \
    release replace-release update-brew

BINARY := .build/release/vz-runner
ENTITLEMENTS := entitlements.plist
VERSION ?= dev

all: sign

build:
	echo 'let buildVersion = "$(VERSION)"' > Sources/vz-runner/version.swift
	swift build -c release

sign: build
	codesign --entitlements $(ENTITLEMENTS) --force -s - --identifier com.olegshirko.vz-runner $(BINARY)

daemon: sign
	$(BINARY) start --share /tmp/anvil-share

stop-daemon:
	@$(BINARY) stop 2>/dev/null || echo "[anvil] daemon not running"

start: daemon

stop: stop-daemon

# -----------------------------------------------------------------------------
# macOS service: LaunchAgent + shell wrapper
# -----------------------------------------------------------------------------

LAUNCHAGENT_LABEL := com.olegshirko.anvil
LAUNCHAGENT_PLIST := scripts/$(LAUNCHAGENT_LABEL).plist
LAUNCHAGENT_DIR := $(HOME)/Library/LaunchAgents
LAUNCHAGENT_DST := $(LAUNCHAGENT_DIR)/$(LAUNCHAGENT_LABEL).plist

service-install:
	@mkdir -p "$(LAUNCHAGENT_DIR)"
	cp "$(LAUNCHAGENT_PLIST)" "$(LAUNCHAGENT_DST)"
	launchctl unload "$(LAUNCHAGENT_DST)" 2>/dev/null || true
	launchctl load "$(LAUNCHAGENT_DST)"
	@echo "[anvil-service] LaunchAgent installed. It will start automatically on next login."

service-uninstall:
	@launchctl unload "$(LAUNCHAGENT_DST)" 2>/dev/null || true
	@rm -f "$(LAUNCHAGENT_DST)"
	@echo "[anvil-service] LaunchAgent uninstalled."

service-start:
	@scripts/anvil-service.sh start

service-stop:
	@scripts/anvil-service.sh stop

service-restart:
	@scripts/anvil-service.sh restart

service-status:
	@scripts/anvil-service.sh status

# Rebuild Swift binary, guest-agent and the containerd initramfs in one go.
rebuild-all: sign initramfs-containerd
	@echo "[anvil] rebuild complete: $(BINARY), guest-agent, initramfs"

# Stop the service, invalidate the saved VM snapshot and restart in debug mode.
# Debug logs from guest-agent are written to $(SHARE_ROOT)/guest-agent.log.
service-debug:
	@$(MAKE) service-stop
	@rm -rf $(HOME)/.anvil-vz/snapshots
	@DEBUG=1 $(MAKE) service-start

# Rebuild everything (Swift binary, guest-agent, initramfs), then restart the
# service in debug mode with a fresh VM snapshot.
service-debug-rebuild: rebuild-all service-debug

# Switch Docker CLI context. LIMA_DOCKER_CONTEXT can be overridden, e.g.
# make docker-context-lima LIMA_DOCKER_CONTEXT=lima.
LIMA_DOCKER_CONTEXT ?= default
LIMA_INSTANCE ?= anvil

docker-context-lima:
	@docker context use $(LIMA_DOCKER_CONTEXT)
	@echo "[anvil] docker context: $(LIMA_DOCKER_CONTEXT)"

docker-context-anvil:
	@docker context use anvil
	@echo "[anvil] docker context: anvil"

# Restart the Lima VM used for initramfs builds and as a fallback docker backend.
lima-restart:
	@limactl stop $(LIMA_INSTANCE) 2>/dev/null || true
	@limactl start $(LIMA_INSTANCE)
	@docker context use $(LIMA_DOCKER_CONTEXT)
	@echo "[anvil] Lima VM '$(LIMA_INSTANCE)' restarted, docker context: $(LIMA_DOCKER_CONTEXT)"

# Start/stop Colima with settings close to anvil (VZ VM + virtiofs mounts).
colima-start:
	@colima start --vm-type=vz --mount-type=virtiofs
	@docker context use colima
	@echo "[anvil] Colima started, docker context: colima"

colima-stop:
	@colima stop
	@echo "[anvil] Colima stopped"

# Prune leftover containers, volumes and images inside the anvil VM.
# Useful because `docker run --rm` is not yet implemented; test leftovers can
# fill the persistent containerd disk and break subsequent compose runs.
prune clean-containers:
	@containers=$$(docker --context anvil ps -aq 2>/dev/null); \
	if [ -n "$$containers" ]; then \
	    docker --context anvil rm -f $$containers >/dev/null; \
	fi
	@docker --context anvil volume prune -f >/dev/null 2>&1 || true
	@docker --context anvil system prune -af --volumes >/dev/null 2>&1 || true
	@echo "[anvil] pruned containers, volumes and images"

clean:
	rm -rf .build .download .venv

test: sign
	$(BINARY) --help || true

time-boot: sign
	python3 scripts/time_boot.py

time-service: sign
	python3 scripts/time_service.py

validate: sign
	python3 scripts/validate_robustness.py

# -----------------------------------------------------------------------------
# Alpine (M0 bare boot)
# -----------------------------------------------------------------------------
ALPINE_URL := https://dl-cdn.alpinelinux.org/alpine/v3.20/releases/aarch64/netboot
ALPINE_DIR := .download/alpine
ALPINE_KERNEL_PE := $(ALPINE_DIR)/vmlinuz-virt
ALPINE_KERNEL_RAW := $(ALPINE_DIR)/vmlinuz-raw
ALPINE_INITRD := $(ALPINE_DIR)/initramfs-virt

$(ALPINE_DIR):
	mkdir -p $(ALPINE_DIR)

download-alpine: $(ALPINE_DIR)
	@if [ ! -f $(ALPINE_KERNEL_PE) ]; then curl -L -o $(ALPINE_KERNEL_PE) $(ALPINE_URL)/vmlinuz-virt; fi
	@if [ ! -f $(ALPINE_INITRD) ]; then curl -L -o $(ALPINE_INITRD) $(ALPINE_URL)/initramfs-virt; fi

extract-alpine-kernel: download-alpine
	@if [ ! -f $(ALPINE_KERNEL_RAW) ]; then \
	    python3 scripts/extract_alpine_kernel.py $(ALPINE_KERNEL_PE) $(ALPINE_KERNEL_RAW).gz && \
	    gunzip -c $(ALPINE_KERNEL_RAW).gz > $(ALPINE_KERNEL_RAW) 2>/dev/null && \
	    file $(ALPINE_KERNEL_RAW); \
	fi

boot-alpine: sign extract-alpine-kernel
	$(BINARY) boot --kernel $(ALPINE_KERNEL_RAW) --initrd $(ALPINE_INITRD)

# -----------------------------------------------------------------------------
# Guest agent + Ubuntu-based initramfs for vsock (M1)
# -----------------------------------------------------------------------------
AGENT_BIN := $(ALPINE_DIR)/guest-agent
ALPINE_AGENT_INITRD := $(ALPINE_DIR)/initramfs-agent

UBUNTU_DIR := .download/ubuntu
UBUNTU_KERNEL := $(UBUNTU_DIR)/vmlinuz-raw
UBUNTU_INITRD := $(UBUNTU_DIR)/initramfs-agent
UBUNTU_INITRD_CONTAINERD := $(UBUNTU_DIR)/initramfs-containerd
UBUNTU_DEB := $(UBUNTU_DIR)/linux-modules.deb
UBUNTU_MODULES_URL := http://ports.ubuntu.com/ubuntu-ports/pool/main/l/linux/linux-modules-6.8.0-124-generic_6.8.0-124.124_arm64.deb

$(UBUNTU_DIR):
	mkdir -p $(UBUNTU_DIR)

download-ubuntu: $(UBUNTU_DIR)
	curl -L -o $(UBUNTU_DIR)/vmlinuz https://cloud-images.ubuntu.com/noble/current/unpacked/noble-server-cloudimg-arm64-vmlinuz-generic
	gunzip -c $(UBUNTU_DIR)/vmlinuz > $(UBUNTU_KERNEL)
	file $(UBUNTU_KERNEL)

ubuntu-modules: $(UBUNTU_DIR)
	@if [ ! -f $(UBUNTU_DEB) ]; then curl -L -o $(UBUNTU_DEB) $(UBUNTU_MODULES_URL); fi

guest-agent:
	cd guest-agent && go mod tidy
	cd guest-agent && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o ../$(AGENT_BIN) .

initramfs-agent: extract-alpine-kernel guest-agent
	scripts/build_initramfs.sh

initramfs-ubuntu: download-ubuntu ubuntu-modules guest-agent
	scripts/build_initramfs_ubuntu.sh

boot: sign initramfs-ubuntu
	$(BINARY) boot --kernel $(UBUNTU_KERNEL) --initrd $(UBUNTU_INITRD) --agent

boot-ubuntu: boot

# -----------------------------------------------------------------------------
# Containerd initramfs (M3)
# -----------------------------------------------------------------------------
CONTAINER_TOOLS_DIR := .download/container-tools
CONTAINERD_URL := https://github.com/containerd/containerd/releases/download/v2.0.0/containerd-static-2.0.0-linux-arm64.tar.gz
NERDCTL_URL := https://github.com/containerd/nerdctl/releases/download/v2.0.4/nerdctl-2.0.4-linux-arm64.tar.gz
RUNC_URL := https://github.com/opencontainers/runc/releases/download/v1.2.0/runc.arm64
CNI_PLUGINS_URL := https://github.com/containernetworking/plugins/releases/download/v1.6.0/cni-plugins-linux-arm64-v1.6.0.tgz
DOCKER_URL := https://download.docker.com/linux/static/stable/aarch64/docker-29.6.1.tgz

$(CONTAINER_TOOLS_DIR):
	mkdir -p $(CONTAINER_TOOLS_DIR)

container-tools: $(CONTAINER_TOOLS_DIR)
	if [ ! -f $(CONTAINER_TOOLS_DIR)/containerd.tgz ]; then \
	    curl -L -o $(CONTAINER_TOOLS_DIR)/containerd.tgz $(CONTAINERD_URL); \
	fi
	if [ ! -f $(CONTAINER_TOOLS_DIR)/nerdctl.tgz ]; then \
	    curl -L -o $(CONTAINER_TOOLS_DIR)/nerdctl.tgz $(NERDCTL_URL); \
	fi
	if [ ! -f $(CONTAINER_TOOLS_DIR)/runc ]; then \
	    curl -L -o $(CONTAINER_TOOLS_DIR)/runc $(RUNC_URL); \
	    chmod +x $(CONTAINER_TOOLS_DIR)/runc; \
	fi
	if [ ! -f $(CONTAINER_TOOLS_DIR)/cni-plugins.tgz ]; then \
	    curl -L -o $(CONTAINER_TOOLS_DIR)/cni-plugins.tgz $(CNI_PLUGINS_URL); \
	fi

# Alpine iptables + libs needed by CNI bridge/portmap/firewall plugins.
ALPINE_IPTABLES_DIR := .download/alpine-iptables
ALPINE_IPTABLES_URL := https://dl-cdn.alpinelinux.org/alpine/v3.20/main/aarch64/iptables-1.8.10-r3.apk
ALPINE_LIBMNL_URL   := https://dl-cdn.alpinelinux.org/alpine/v3.20/main/aarch64/libmnl-1.0.5-r2.apk
ALPINE_LIBNFTNL_URL := https://dl-cdn.alpinelinux.org/alpine/v3.20/main/aarch64/libnftnl-1.2.6-r0.apk
ALPINE_LIBXTABLES_URL := https://dl-cdn.alpinelinux.org/alpine/v3.20/main/aarch64/libxtables-1.8.10-r3.apk

$(ALPINE_IPTABLES_DIR):
	mkdir -p $(ALPINE_IPTABLES_DIR)

alpine-iptables: $(ALPINE_IPTABLES_DIR)
	if [ ! -f $(ALPINE_IPTABLES_DIR)/iptables.apk ]; then \
	    curl -L -o $(ALPINE_IPTABLES_DIR)/iptables.apk $(ALPINE_IPTABLES_URL); \
	fi
	if [ ! -f $(ALPINE_IPTABLES_DIR)/libmnl.apk ]; then \
	    curl -L -o $(ALPINE_IPTABLES_DIR)/libmnl.apk $(ALPINE_LIBMNL_URL); \
	fi
	if [ ! -f $(ALPINE_IPTABLES_DIR)/libnftnl.apk ]; then \
	    curl -L -o $(ALPINE_IPTABLES_DIR)/libnftnl.apk $(ALPINE_LIBNFTNL_URL); \
	fi
	if [ ! -f $(ALPINE_IPTABLES_DIR)/libxtables.apk ]; then \
	    curl -L -o $(ALPINE_IPTABLES_DIR)/libxtables.apk $(ALPINE_LIBXTABLES_URL); \
	fi

initramfs-containerd: extract-alpine-kernel ubuntu-modules guest-agent container-tools alpine-iptables
	@if command -v limactl >/dev/null 2>&1 && limactl list anvil --format '{{.Status}}' 2>/dev/null | grep -q Running; then \
	    echo "Building initramfs inside Lima VM 'anvil'..."; \
	    limactl shell anvil -- bash $(CURDIR)/scripts/build_initramfs_containerd.sh; \
	else \
	    echo "Lima VM 'anvil' not running; falling back to local Docker (requires Docker Desktop)."; \
	    scripts/build_initramfs_containerd.sh; \
	fi

# Default boot path does NOT rebuild the initramfs so that an existing snapshot's
# config hash stays valid. Build the initramfs explicitly with
# `make initramfs-containerd` or use `make boot-containerd-fresh`.
boot-containerd: sign
	@if [ ! -f $(UBUNTU_INITRD_CONTAINERD) ]; then \
	    echo "initramfs not found; run 'make initramfs-containerd' first"; exit 1; \
	fi
	$(BINARY) boot --kernel $(UBUNTU_KERNEL) --initrd $(UBUNTU_INITRD_CONTAINERD) --agent --share /tmp/anvil-share

# Explicit cold-boot target that rebuilds the initramfs first.
boot-containerd-fresh: sign initramfs-containerd
	$(BINARY) boot --kernel $(UBUNTU_KERNEL) --initrd $(UBUNTU_INITRD_CONTAINERD) --agent --share /tmp/anvil-share --fresh

# -----------------------------------------------------------------------------
# Bench harness
# -----------------------------------------------------------------------------

# Backends to benchmark. Override, e.g.:
#   make bench BENCH_BACKENDS="vz-runner lima colima"
BENCH_BACKENDS ?= vz-runner

# Prepull harness workload images into the vz-runner VM. Starts the service if
# it is not running, because prepull needs a live guest-agent/containerd.
harness-prepull: sign
	@$(MAKE) service-start
	@VZRUNNER_BIN="$(CURDIR)/$(BINARY)" bash "$(CURDIR)/bench-harness/scripts/prepull.sh" vz-runner

# Run harness benchmarks. Stops the service first so the harness can start its
# own isolated daemon without control-socket/lock conflicts.
harness harness-tests: sign
	@$(MAKE) service-stop
	@VZRUNNER_BIN="$(CURDIR)/$(BINARY)" bash "$(CURDIR)/bench-harness/run_bench.sh" $(BENCH_BACKENDS)

# Run harness against all supported backends. Requires each backend to be
# installed (Lima, Colima, OrbStack, Docker Desktop) and its docker context to
# exist. Results are merged into the same CSV and latest.md as a single run.
ALL_BENCH_BACKENDS ?= vz-runner lima colima orbstack docker-desktop

bench-all harness-all: sign
	@$(MAKE) service-stop
	@VZRUNNER_BIN="$(CURDIR)/$(BINARY)" bash "$(CURDIR)/bench-harness/run_bench.sh" $(ALL_BENCH_BACKENDS)

# -----------------------------------------------------------------------------
# Release management
# -----------------------------------------------------------------------------
VERSION ?=

release: sign
	@test -n "$(VERSION)" || { echo "Usage: make release VERSION=x.y.z"; exit 1; }
	@git rev-parse "v$(VERSION)" >/dev/null 2>&1 && \
		{ echo "Error: tag v$(VERSION) already exists. Use 'make replace-release VERSION=$(VERSION)'."; exit 1; } || true
	@echo "[release] tagging v$(VERSION) and pushing..."
	git tag -a "v$(VERSION)" -m "v$(VERSION)"
	git push origin "v$(VERSION)"
	@echo "[release] waiting for CI to publish release..."
	@AUTH=$$(test -n "$$GITHUB_TOKEN" && echo "-H 'Authorization: token $$GITHUB_TOKEN'" || echo ""); \
	i=0; while [ $$i -lt 20 ]; do \
		sleep 15; \
		curl -sf $$AUTH "https://api.github.com/repos/olegshirko/anvil/releases/tags/v$(VERSION)" >/dev/null 2>&1 && break; \
		i=$$((i + 1)); printf "."; \
	done; echo ""
	@AUTH=$$(test -n "$$GITHUB_TOKEN" && echo "-H 'Authorization: token $$GITHUB_TOKEN'" || echo ""); \
	curl -sf $$AUTH "https://api.github.com/repos/olegshirko/anvil/releases/tags/v$(VERSION)" >/dev/null 2>&1 || \
		{ echo "[release] timeout. Check https://github.com/olegshirko/anvil/actions"; exit 1; }
	@echo "[release] release published."
	$(MAKE) update-brew VERSION=$(VERSION)

replace-release: sign
	@test -n "$(VERSION)" || { echo "Usage: make replace-release VERSION=x.y.z"; exit 1; }
	@echo "[replace-release] deleting remote tag and GitHub release..."
	git push origin ":refs/tags/v$(VERSION)" 2>/dev/null || true
	git tag -d "v$(VERSION)" 2>/dev/null || true
	gh release delete "v$(VERSION)" --repo olegshirko/anvil --yes 2>/dev/null || \
		echo "[replace-release] note: gh not authed or release not found (run: gh auth login)"
	@echo "[replace-release] tagging v$(VERSION) and pushing..."
	git tag -a "v$(VERSION)" -m "v$(VERSION)"
	git push origin "v$(VERSION)"
	@echo "[replace-release] waiting for CI to publish release..."
	@AUTH=$$(test -n "$$GITHUB_TOKEN" && echo "-H 'Authorization: token $$GITHUB_TOKEN'" || echo ""); \
	i=0; while [ $$i -lt 20 ]; do \
		sleep 15; \
		curl -sf $$AUTH "https://api.github.com/repos/olegshirko/anvil/releases/tags/v$(VERSION)" >/dev/null 2>&1 && break; \
		i=$$((i + 1)); printf "."; \
	done; echo ""
	@AUTH=$$(test -n "$$GITHUB_TOKEN" && echo "-H 'Authorization: token $$GITHUB_TOKEN'" || echo ""); \
	curl -sf $$AUTH "https://api.github.com/repos/olegshirko/anvil/releases/tags/v$(VERSION)" >/dev/null 2>&1 || \
		{ echo "[replace-release] timeout. Check https://github.com/olegshirko/anvil/actions"; exit 1; }
	@echo "[replace-release] release published."
	$(MAKE) update-brew VERSION=$(VERSION)

update-brew:
	@test -n "$(VERSION)" || { echo "Usage: make update-brew VERSION=x.y.z"; exit 1; }
	@echo "[update-brew] downloading tar.gz and computing sha256..."
	@SHA_TAR=$$(curl -sL "https://github.com/olegshirko/anvil/releases/download/v$(VERSION)/anvil-darwin-arm64.tar.gz" | shasum -a 256 | cut -d' ' -f1); \
	echo "[update-brew] sha256=$$SHA_TAR"; \
	sed -i '' 's|version ".*"|version "$(VERSION)"|' /tmp/homebrew-tap/anvil.rb; \
	sed -i '' 's|url ".*anvil-darwin-arm64.tar.gz"|url "https://github.com/olegshirko/anvil/releases/download/v$(VERSION)/anvil-darwin-arm64.tar.gz"|' /tmp/homebrew-tap/anvil.rb; \
	sed -i '' 's|sha256 ".*"|sha256 "'$$SHA_TAR'"|' /tmp/homebrew-tap/anvil.rb; \
	cd /tmp/homebrew-tap && git add anvil.rb && git commit -m "v$(VERSION)" && git push; \
	echo "[update-brew] done."
