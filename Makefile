# Run all targets from a WSL terminal, inside this folder.
BEEVM_HOME ?= $(HOME)/.beevm
export BEEVM_HOME
GO ?= /usr/local/go/bin/go

.PHONY: help setup images kernel rootfs rootfs-python rootfs-node boot boot-test clean-images build examples test test-kvm test-isolation test-s3 vet minio minio-up minio-down monitoring monitoring-up monitoring-down

help:
	@echo "make setup        one-time install (Go, Firecracker, kernel, rootfs); needs sudo"
	@echo "make images       (re)build kernel + all rootfs images (binary, python, node)"
	@echo "make boot         boot a MicroVM with the shell script and get a shell (reboot -f to exit)"
	@echo "make boot-test    boot with the shell script, check it worked, print boot time"
	@echo "make build        build bin/beevmd (server) and bin/beevm (user CLI)"
	@echo "make examples     build example functions (static) into bin/functions/"
	@echo "make test         Go unit tests (no KVM needed)"
	@echo "make test-kvm     Go tests that boot real MicroVMs (needs KVM)"
	@echo "make test-isolation  isolation tests (jailer, cgroups, network, attacks); asks for sudo"
	@echo "make test-s3     store tests against a real MinIO (start make minio first)"
	@echo "make minio        build (first time) and run a local MinIO S3 server, no Docker"
	@echo "make minio-up     same, with Docker Compose (minio-down stops it)"
	@echo "make monitoring   (optional) run Prometheus :9090 + Grafana :3000 for beevmd, no Docker"
	@echo "make vet          go vet"
	@echo "make clean-images delete images and run folders in $(BEEVM_HOME)"

setup:
	bash scripts/setup-wsl.sh

images: kernel rootfs rootfs-python rootfs-node
kernel:
	bash guest/kernel/fetch-kernel.sh
rootfs:
	bash guest/rootfs/build-rootfs.sh binary
rootfs-python:
	bash guest/rootfs/build-rootfs.sh python
rootfs-node:
	bash guest/rootfs/build-rootfs.sh node

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
	@for f in hello-go-binary testfn forkbomb memhog cpuhog netprobe hostread; do \
	  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags="-s -w" -o bin/functions/$$f ./examples/functions/$$f || exit 1; \
	  echo "built bin/functions/$$f"; \
	done

test:
	$(GO) test ./...

test-kvm:
	$(GO) test -tags kvm -v -count=1 ./internal/...

# Compiled as you (Go's build cache stays yours), then run as root by the script.
# RUN=<regexp> picks tests, e.g. make test-isolation RUN=TestJailed
ISOLATION_PKGS = isolation api
test-isolation: examples
	@mkdir -p bin/test && rm -f bin/test/*.test
	@for p in $(ISOLATION_PKGS); do $(GO) test -c -tags kvm -o bin/test/$$p.test ./internal/$$p || exit 1; done
	sudo BEEVM_HOME=$(BEEVM_HOME) RUN='$(RUN)' bash scripts/test-isolation.sh

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

# Optional module (M10): Prometheus + Grafana. Delete this block, scripts/monitoring.sh and
# deploy/monitoring/ to remove it; nothing else depends on them.
monitoring:
	bash scripts/monitoring.sh run

monitoring-up:
	docker compose -f deploy/docker-compose.yml --profile monitoring up -d prometheus grafana

monitoring-down:
	docker compose -f deploy/docker-compose.yml --profile monitoring stop prometheus grafana
