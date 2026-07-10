.PHONY: all build sign clean test \
    daemon stop-daemon \
    boot boot-alpine boot-ubuntu boot-containerd \
    download-alpine extract-alpine-kernel \
    download-ubuntu ubuntu-modules \
    guest-agent initramfs-agent initramfs-ubuntu initramfs-containerd \
    container-tools time-boot time-service validate

BINARY := .build/release/vz-runner
ENTITLEMENTS := entitlements.plist

all: sign

build:
	swift build -c release

sign: build
	codesign --entitlements $(ENTITLEMENTS) --force -s - --identifier com.olegshirko.vz-runner $(BINARY)

daemon: sign
	$(BINARY) daemon --share /tmp/anvil-share

stop-daemon:
	@PID=$$(cat ~/.anvil-vz/daemon.pid 2>/dev/null); \
	if [ -n "$$PID" ]; then \
	    kill $$PID 2>/dev/null && echo "[vz-runner] daemon stopped" || echo "[vz-runner] daemon not running"; \
	else \
	    echo "[vz-runner] no daemon pid file"; \
	fi

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
	curl -L -o $(ALPINE_KERNEL_PE) $(ALPINE_URL)/vmlinuz-virt
	curl -L -o $(ALPINE_INITRD) $(ALPINE_URL)/initramfs-virt

extract-alpine-kernel: download-alpine
	python3 scripts/extract_alpine_kernel.py $(ALPINE_KERNEL_PE) $(ALPINE_KERNEL_RAW).gz
	-gunzip -c $(ALPINE_KERNEL_RAW).gz > $(ALPINE_KERNEL_RAW) 2>/dev/null
	file $(ALPINE_KERNEL_RAW)

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
	curl -L -o $(UBUNTU_DEB) $(UBUNTU_MODULES_URL)

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
NERDCTL_URL := https://github.com/containerd/nerdctl/releases/download/v2.0.0/nerdctl-2.0.0-linux-arm64.tar.gz
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
	scripts/build_initramfs_containerd.sh

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
