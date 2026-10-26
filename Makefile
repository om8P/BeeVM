# Run all targets from a WSL terminal, inside this folder.
BEEVM_HOME ?= $(HOME)/.beevm
export BEEVM_HOME
GO ?= /usr/local/go/bin/go

.PHONY: help setup images kernel rootfs boot boot-test clean-images build examples test test-kvm test-s3 vet minio minio-up minio-down

help:
	@echo "make setup        one-time install (Go, Firecracker, kernel, rootfs); needs sudo"
	@echo "make images       (re)build kernel + rootfs only"
	@echo "make boot         boot a MicroVM with the shell script and get a shell (reboot -f to exit)"
	@echo "make boot-test    boot with the shell script, check it worked, print boot time"
	@echo "make build        build bin/beevmd (server) and bin/beevm (user CLI)"
	@echo "make examples     build example functions (static) into bin/functions/"
	@echo "make test         Go unit tests (no KVM needed)"
	@echo "make test-kvm     Go tests that boot real MicroVMs (needs KVM)"
	@echo "make test-s3     store tests against a real MinIO (start make minio first)"
	@echo "make minio        build (first time) and run a local MinIO S3 server, no Docker"
	@echo "make minio-up     same, with Docker Compose (minio-down stops it)"
	@echo "make vet          go vet"
	@echo "make clean-images delete images and run folders in $(BEEVM_HOME)"

setup:
	bash scripts/setup-wsl.sh

images: kernel rootfs
kernel:
	bash guest/kernel/fetch-kernel.sh
rootfs:
	bash guest/rootfs/build-rootfs.sh

boot:
	bash scripts/boot-vm.sh

boot-test:
	bash scripts/boot-vm.sh --selftest

build:
	$(GO) build -o bin/beevmd ./cmd/beevmd
	@if [ -d cmd/beevm ]; then $(GO) build -o bin/beevm ./cmd/beevm; fi

# Functions run inside an Alpine guest (musl), so they must be static: CGO_ENABLED=0.
examples:
	@mkdir -p bin/functions
	@for f in hello-go-binary testfn; do \
	  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags="-s -w" -o bin/functions/$$f ./examples/functions/$$f || exit 1; \
	  echo "built bin/functions/$$f"; \
	done

test:
	$(GO) test ./...

test-kvm:
	$(GO) test -tags kvm -v -count=1 ./internal/...

# Needs `make minio` (or minio-up) running in another terminal.
test-s3:
	set -a; . deploy/minio.env; set +a; 	BEEVM_TEST_S3_ENDPOINT=$$BEEVM_S3_ENDPOINT BEEVM_TEST_S3_KEY=$$AWS_ACCESS_KEY_ID BEEVM_TEST_S3_SECRET=$$AWS_SECRET_ACCESS_KEY 	$(GO) test -count=1 -v ./internal/store/

minio:
	bash scripts/minio.sh run

minio-up:
	docker compose -f deploy/docker-compose.yml up -d

minio-down:
	docker compose -f deploy/docker-compose.yml down

vet:
	$(GO) vet ./...

clean-images:
	rm -rf $(BEEVM_HOME)/images $(BEEVM_HOME)/run
