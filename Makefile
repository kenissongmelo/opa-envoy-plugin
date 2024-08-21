# Copyright 2018 The OPA Authors. All rights reserved.
# Use of this source code is governed by an Apache2
# license that can be found in the LICENSE file.

VERSION_OPA := $(shell ./build/get-opa-version.sh)
VERSION := $(VERSION_OPA)-envoy$(shell ./build/get-plugin-rev.sh)
VERSION_ISTIO := $(VERSION_OPA)-istio$(shell ./build/get-plugin-rev.sh)

PACKAGES := $(shell go list ./.../ | grep -v 'vendor')

DOCKER := docker

DOCKER_UID ?= 0
DOCKER_GID ?= 0

CGO_ENABLED ?= 1
WASM_ENABLED ?= 1
GOARCH ?= $(shell go env GOARCH)

# GOPROXY=off: Don't pull anything off the network
# see https://github.com/thepudds/go-module-knobs/blob/master/README.md
GO := CGO_ENABLED=$(CGO_ENABLED) GOARCH=$(GOARCH) GO111MODULE=on GOFLAGS=-mod=vendor GOPROXY=off go
GOVERSION := $(shell cat ./.go-version)
GOOS := $(shell go env GOOS)
DISABLE_CGO := CGO_ENABLED=0

BIN := opa_envoy_$(GOOS)_$(GOARCH)

REPOSITORY ?= openpolicyagent
IMAGE := $(REPOSITORY)/opa

GO_TAGS := -tags=
ifeq ($(WASM_ENABLED),1)
GO_TAGS = -tags=opa_wasm
endif

ifeq ($(shell tty > /dev/null && echo 1 || echo 0), 1)
DOCKER_FLAGS := --rm -it
else
DOCKER_FLAGS := --rm
endif

RELEASE_BUILD_IMAGE := golang:$(GOVERSION)

RELEASE_DIR ?= _release/$(VERSION)

BUILD_COMMIT := $(shell ./build/get-build-commit.sh)
BUILD_TIMESTAMP := $(shell ./build/get-build-timestamp.sh)
BUILD_HOSTNAME := $(shell ./build/get-build-hostname.sh)

LDFLAGS := "-X github.com/open-policy-agent/opa/version.Version=$(VERSION) \
	-X github.com/open-policy-agent/opa/version.Vcs=$(BUILD_COMMIT) \
	-X github.com/open-policy-agent/opa/version.Timestamp=$(BUILD_TIMESTAMP) \
	-X github.com/open-policy-agent/opa/version.Hostname=$(BUILD_HOSTNAME)"
	
# BuildKit is required for automatic platform arg injection (see Dockerfile)
export DOCKER_BUILDKIT := 1

ifeq ($(GOOS)/$(GOARCH),darwin/arm64)
WASM_ENABLED=0
endif

DOCKER_PLATFORMS := linux/amd64,linux/arm64

######################################################
#
# Development targets
#
######################################################

.PHONY: all
all: build test check

.PHONY: version
version:
	@echo $(VERSION)

.PHONY: generate
generate:
	$(GO) generate ./...

.PHONY: build
build: generate
	$(GO) build $(GO_TAGS) -o $(BIN) -ldflags $(LDFLAGS) ./cmd/opa-envoy-plugin/...

.PHONY: build-darwin
build-darwin:
	@$(MAKE) build GOOS=darwin

.PHONY: build-linux
build-linux: ensure-release-dir ensure-linux-toolchain
	@$(MAKE) build GOOS=linux

.PHONY: build-linux-static
build-linux-static: ensure-release-dir ensure-linux-toolchain
	@$(MAKE) build GOOS=linux WASM_ENABLED=0 CGO_ENABLED=0

.PHONY: build-windows
build-windows:
	@$(MAKE) build GOOS=windows

.PHONY: image
image:
	@$(MAKE) ci-go-build-linux
	@$(MAKE) image-quick

.PHONY: start-builder
start-builder:
	@./build/buildx_workaround.sh

.PHONY: image-static
image-static:
	CGO_ENABLED=0 WASM_ENABLED=0 $(MAKE) ci-go-build-linux-static
	@$(MAKE) image-quick-static

.PHONY: image-quick
image-quick:
	$(MAKE) image-quick-$(GOARCH)

# % = arch
.PHONY: image-quick-%
image-quick-%:
ifneq ($(GOARCH),arm64) # build only static images for arm64
	$(DOCKER) build \
		-f Dockerfile \
		-t $(IMAGE):$(VERSION) \
		--build-arg BASE=chainguard/glibc-dynamic \
		--build-arg BIN_DIR=$(RELEASE_DIR) \
		--platform linux/$* \
		.

endif
	$(DOCKER) build \
		-f Dockerfile \
		-t $(IMAGE):$(VERSION)-static \
		--build-arg BASE=chainguard/static:latest \
		--build-arg BIN_DIR=$(RELEASE_DIR) \
		--build-arg BIN_SUFFIX=_static \
		--platform linux/$* \
		.

.PHONY: image-quick-static
image-quick-static:
	$(MAKE) image-quick-static-$(GOARCH)

.PHONY: image-quick-static-%
image-quick-static-%:
	$(DOCKER) build --platform=linux/$* --push -t $(IMAGE):$(VERSION)-static --build-arg BASE=chainguard/static:latest -f Dockerfile .

.PHONY: push-manifest-list
push-manifest-list:
	$(DOCKER) buildx build --platform=$(DOCKER_PLATFORMS) \
		--push -t $(IMAGE):$(VERSION) \
		--build-arg BASE=chainguard/glibc-dynamic:latest \
		-f Dockerfile .
	
	$(DOCKER) buildx build --platform=$(DOCKER_PLATFORMS) \
		--push -t $(IMAGE):$(VERSION_ISTIO) \
		--build-arg BASE=chainguard/glibc-dynamic:latest \
		-f Dockerfile .

.PHONY: push-manifest-list-latest
push-manifest-list-latest:
	$(DOCKER) buildx build --platform=$(DOCKER_PLATFORMS) \
		--push -t $(IMAGE):latest-envoy \
		--build-arg BASE=chainguard/static:latest \
		-f Dockerfile .

	$(DOCKER) buildx build --platform=$(DOCKER_PLATFORMS) \
		--push -t $(IMAGE):latest-istio \
		--build-arg BASE=chainguard/static:latest \
		-f Dockerfile .

.PHONY: push-manifest-list-static
push-manifest-list-static:
	$(DOCKER) buildx build --platform=$(DOCKER_PLATFORMS) \
		--push -t $(IMAGE):$(VERSION)-static \
		--build-arg BASE=chainguard/static:latest \
		-f Dockerfile .

	$(DOCKER) buildx build --platform=$(DOCKER_PLATFORMS) \
		--push -t $(IMAGE):$(VERSION_ISTIO)-static \
		--build-arg BASE=chainguard/static:latest \
		-f Dockerfile .

.PHONY: docker-login
docker-login:
	@echo "Docker Login..."
	@echo ${DOCKER_PASSWORD} | $(DOCKER) login -u ${DOCKER_USER} --password-stdin

.PHONY: deploy-ci
deploy-ci: ensure-release-dir start-builder ci-build-binaries docker-login push-manifest-list-latest ci-build-binaries-static push-manifest-list-static

.PHONY: test
test: generate
	$(DISABLE_CGO) $(GO) test -v -bench=. $(PACKAGES)

.PHONY: test-e2e
test-e2e:
	bats -t test/bats/test.bats

.PHONY: test-cluster
test-cluster:
	@./build/install-istio-with-kind.sh

.PHONY: clean
clean:
	rm -f .Dockerfile_*
	rm -f opa_*_*
	rm -f *.so

.PHONY: check
check: check-fmt check-vet check-lint

.PHONY: check-fmt
check-fmt:
	./build/check-fmt.sh

.PHONY: check-vet
check-vet:
	./build/check-vet.sh

.PHONY: check-lint
check-lint:
	./build/check-lint.sh

.PHONY: generatepb
generatepb:
	protoc --proto_path=test/files \
	  --descriptor_set_out=test/files/combined.pb \
	  --include_imports \
	  test/files/example/Example.proto \
	  test/files/book/Book.proto

CI_GOLANG_DOCKER_MAKE := $(DOCKER) run \
        $(DOCKER_FLAGS) \
        -u $(DOCKER_UID):$(DOCKER_GID) \
        -v $(PWD):/src \
        -w /src \
        -e GOCACHE=/src/.go/cache \
        -e CGO_ENABLED=$(CGO_ENABLED) \
        -e WASM_ENABLED=$(WASM_ENABLED) \
        -e TELEMETRY_URL=$(TELEMETRY_URL) \
		-e GOARCH=$(GOARCH) \
        golang:$(GOVERSION) \
		make

.PHONY: ensure-linux-toolchain
ensure-linux-toolchain:
ifeq ($(CGO_ENABLED),1)
	$(eval export CC = $(shell GOARCH=$(GOARCH) build/ensure-linux-toolchain.sh))
else
	@echo "CGO_ENABLED=$(CGO_ENABLED). No need to check gcc toolchain."
endif

.PHONY: ci-go-%
ci-go-%:
	$(CI_GOLANG_DOCKER_MAKE) "$*"

.PHONY: ci-build-binaries
ci-build-binaries: ensure-linux-toolchain
	$(MAKE) ci-go-build-linux GOARCH=arm64
	$(MAKE) ci-go-build-linux GOARCH=amd64

.PHONY: ci-build-binaries-static
ci-build-binaries-static: ensure-linux-toolchain
	$(MAKE) ci-go-build-linux-static GOARCH=arm64
	$(MAKE) ci-go-build-linux-static GOARCH=amd64

.PHONY: tag-latest
tag-latest:
	docker tag $(IMAGE):$(VERSION) $(IMAGE):latest-envoy
	docker tag $(IMAGE):$(VERSION) $(IMAGE):latest-istio

.PHONY: tag-latest-static
tag-latest-static:
	docker tag $(IMAGE):$(VERSION)-static $(IMAGE):latest-envoy-static
	docker tag $(IMAGE):$(VERSION)-static $(IMAGE):latest-istio-static

.PHONY: release
release:
	$(DOCKER) run $(DOCKER_FLAGS) \
		-v $(PWD)/$(RELEASE_DIR):/$(RELEASE_DIR) \
		-v $(PWD):/_src \
		$(RELEASE_BUILD_IMAGE) \
		/_src/build/build-release.sh --version=$(VERSION) --output-dir=/$(RELEASE_DIR) --source-url=/_src

.PHONY: release-build-linux
release-build-linux: ensure-release-dir
	@$(MAKE) build GOOS=linux CGO_ENABLED=0 WASM_ENABLED=0
	mv opa_envoy_linux_$(GOARCH) $(RELEASE_DIR)/

.PHONY: release-build-darwin
release-build-darwin: ensure-release-dir
	@$(MAKE) build GOOS=darwin CGO_ENABLED=0 WASM_ENABLED=0
	mv opa_envoy_darwin_$(GOARCH) $(RELEASE_DIR)/

.PHONY: release-build-windows
release-build-windows: ensure-release-dirart
	@$(MAKE) build GOOS=windows CGO_ENABLED=0 WASM_ENABLED=0
	mv opa_envoy_windows_$(GOARCH) $(RELEASE_DIR)/opa_envoy_windows_$(GOARCH).exe

.PHONY: ensure-release-dir
ensure-release-dir:
	mkdir -p $(RELEASE_DIR)

.PHONY: build-all-platforms
build-all-platforms: release-build-linux release-build-darwin release-build-windows
