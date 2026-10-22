# Run all targets from a WSL terminal, inside this folder.
BEEVM_HOME ?= $(HOME)/.beevm
export BEEVM_HOME

.PHONY: help setup images kernel rootfs boot boot-test clean-images

help:
	@echo "make setup        one-time install (Go, Firecracker, kernel, rootfs); needs sudo"
	@echo "make images       (re)build kernel + rootfs only"
	@echo "make boot         boot a MicroVM and get a shell (type 'reboot -f' to exit)"
	@echo "make boot-test    boot automatically, check it worked, print boot time"
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

clean-images:
	rm -rf $(BEEVM_HOME)/images $(BEEVM_HOME)/run
