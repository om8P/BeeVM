package vmm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeConfig makes a Config whose files exist, so Validate passes without KVM or Firecracker.
func fakeConfig(t *testing.T) Config {
	t.Helper()
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	return Config{
		BaseDir:        filepath.Join(dir, "vms"),
		FirecrackerBin: write("firecracker", "#!/bin/sh\n"),
		KernelPath:     write("vmlinux", "kernel"),
		RootfsPath:     write("rootfs.ext4", "rootfs-bytes"),
	}
}

func TestDefaults(t *testing.T) {
	cfg, err := fakeConfig(t).applyDefaults()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.ID) != 8 {
		t.Errorf("want 8-char random id, got %q", cfg.ID)
	}
	if cfg.VCPUs != 1 || cfg.MemMiB != 128 {
		t.Errorf("want 1 vCPU / 128 MiB, got %d / %d", cfg.VCPUs, cfg.MemMiB)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("defaults should validate: %v", err)
	}
}

func TestValidateRejectsBadConfig(t *testing.T) {
	good, _ := fakeConfig(t).applyDefaults()
	cases := map[string]func(*Config){
		"missing basedir": func(c *Config) { c.BaseDir = "" },
		"slash in id":     func(c *Config) { c.ID = "a/b" },
		"zero vcpus":      func(c *Config) { c.VCPUs = 0 },
		"tiny memory":     func(c *Config) { c.MemMiB = 16 },
		"reserved cid":    func(c *Config) { c.VsockCID = 2 },
		"missing kernel":  func(c *Config) { c.KernelPath = "/nonexistent/vmlinux" },
		"empty rootfs":    func(c *Config) { c.RootfsPath = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := good
			mutate(&c)
			if err := c.Validate(); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

func TestBootArgs(t *testing.T) {
	c := Config{}
	if c.BootArgs() != DefaultBootArgs {
		t.Errorf("got %q", c.BootArgs())
	}
	c.ExtraBootArgs = "beevm.mode=idle"
	if !strings.HasSuffix(c.BootArgs(), " beevm.mode=idle") || !strings.HasPrefix(c.BootArgs(), DefaultBootArgs) {
		t.Errorf("got %q", c.BootArgs())
	}
}

func TestNewCreatesFolderAndCopiesRootfs(t *testing.T) {
	cfg := fakeConfig(t)
	vm, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(vm.Dir(), "rootfs.ext4"))
	if err != nil || string(got) != "rootfs-bytes" {
		t.Fatalf("rootfs copy wrong: %q, %v", got, err)
	}
	// The base image must be untouched and separate from the copy.
	if err := os.WriteFile(filepath.Join(vm.Dir(), "rootfs.ext4"), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if base, _ := os.ReadFile(cfg.RootfsPath); string(base) != "rootfs-bytes" {
		t.Error("base rootfs was modified")
	}
}

func TestNewWithBadConfigLeavesNothingBehind(t *testing.T) {
	cfg := fakeConfig(t)
	cfg.KernelPath = "/nonexistent"
	if _, err := New(cfg); err == nil {
		t.Fatal("expected error")
	}
	if _, err := os.Stat(cfg.BaseDir); err == nil {
		t.Error("BaseDir should not have been created")
	}
}

func TestLifecycleCallsBeforeStartAreSafe(t *testing.T) {
	vm, err := New(fakeConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if graceful, err := vm.Stop(context.Background()); !graceful || err != nil {
		t.Errorf("Stop before Start: graceful=%v err=%v", graceful, err)
	}
	vm.Kill() // must not hang or panic
	if vm.PID() != 0 {
		t.Error("PID should be 0 before start")
	}
	if _, err := vm.WaitForConsole(context.Background(), "X"); err == nil {
		t.Error("WaitForConsole before Start should fail")
	}
}

func TestCleanupRemovesFolder(t *testing.T) {
	vm, _ := New(fakeConfig(t))
	if err := vm.Cleanup(false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(vm.Dir()); !os.IsNotExist(err) {
		t.Error("folder should be gone")
	}
	if err := vm.Cleanup(false); err != nil { // calling twice is fine
		t.Errorf("second Cleanup: %v", err)
	}
}

func TestCleanupKeepLogsKeepsOnlyConsole(t *testing.T) {
	vm, _ := New(fakeConfig(t))
	if err := os.WriteFile(vm.ConsolePath(), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := vm.Cleanup(true); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(vm.Dir())
	if len(entries) != 1 || entries[0].Name() != "console.log" {
		t.Errorf("want only console.log, got %v", entries)
	}
}

func TestConsoleHasLine(t *testing.T) {
	p := filepath.Join(t.TempDir(), "console.log")
	if found, err := consoleHasLine(p, "X"); found || err != nil {
		t.Errorf("missing file: found=%v err=%v", found, err)
	}
	// Serial consoles end lines with \r\n; a marker inside a longer line must not match.
	if err := os.WriteFile(p, []byte("boot\r\nBEEVM_READY_NOT\r\n  BEEVM_READY \r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if found, _ := consoleHasLine(p, "BEEVM_READY"); !found {
		t.Error("marker line should match")
	}
	if found, _ := consoleHasLine(p, "BEEVM"); found {
		t.Error("partial line must not match")
	}
}
