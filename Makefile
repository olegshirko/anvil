OS      ?= $(shell uname)
ARCH    ?= $(shell uname -m)
GOOS    ?= $(shell echo "$(OS)" | tr '[:upper:]' '[:lower:]')

ifeq ($(GOOS),darwin)
	CGO_ENABLED ?= 1
endif

GOARCH_x86_64  = amd64
GOARCH_aarch64 = arm64
GOARCH_arm64   = arm64
GOARCH         ?= $(shell echo "$(GOARCH_$(ARCH))")

VERSION  := $(shell git describe --tags --always)
REVISION := $(shell git rev-parse HEAD)
PKG       = anvil/internal/usecase
LD_FLAGS  = -X $(PKG).appVersion=$(VERSION) -X $(PKG).revision=$(REVISION)

OUT_DIR  = _output/binaries
OUT_BIN  = anvil-$(OS)-$(ARCH)
INSTALL  = /usr/local/bin
NAME     = anvil

.PHONY: all build clean fmt test lint install vmnet integration generate images-sha print-binary-name nix-shell

all: build

clean:
	rm -rf _output _build

fmt:
	go fmt ./...
	goimports -w .

build:
	GOOS=$(GOOS) GOARCH=$(GOARCH) go build -ldflags="$(LD_FLAGS)" -o $(OUT_DIR)/$(OUT_BIN) ./cmd/anvil
ifeq ($(GOOS),darwin)
	codesign -s - --force $(OUT_DIR)/$(OUT_BIN)
endif
	cd $(OUT_DIR) && openssl sha256 -r -out $(OUT_BIN).sha256sum $(OUT_BIN)

test:
	go test -v -ldflags="$(LD_FLAGS)" ./...

lint:
	golangci-lint --timeout 3m run

install:
	mkdir -p $(INSTALL)
	cp $(OUT_DIR)/$(OUT_BIN) $(INSTALL)/$(NAME)
	chmod +x $(INSTALL)/$(NAME)

vmnet:
	sh scripts/build_vmnet.sh

integration: build
	GOARCH=$(GOARCH) anvil_BINARY=$(OUT_DIR)/$(OUT_BIN) scripts/integration.sh

generate:
	go generate ./...

images-sha:
	cd internal/embedded/images && go run update_manifest.go

update-manifest-gh:
	@command -v gh >/dev/null 2>&1 || { echo "gh CLI is required (https://cli.github.com)"; exit 1; }
	cd internal/embedded/images && go run update_manifest_gh.go

print-binary-name:
	@echo $(OUT_DIR)/$(OUT_BIN)

nix-shell:
	$(eval DERIVATION=$(shell nix-build))
	echo $(DERIVATION) | grep ^/nix
	nix-shell -p $(DERIVATION)
