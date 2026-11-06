package vmm

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func jailedConfig(t *testing.T) Config {
	t.Helper()
	dir := t.TempDir()
	for _, f := range []string{"firecracker", "jailer", "vmlinux", "rootfs.ext4"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return Config{
		ID: "abc123", BaseDir: filepath.Join(dir, "run"), FirecrackerBin: filepath.Join(dir, "firecracker"),
		KernelPath: filepath.Join(dir, "vmlinux"), RootfsPath: filepath.Join(dir, "rootfs.ext4"), SharedRootfs: true,
		VCPUs: 1, MemMiB: 128, VsockCID: 3,
		Jail: &Jail{JailerBin: filepath.Join(dir, "jailer"), ChrootBase: filepath.Join(dir, "jail"), UID: 900005, GID: 900005,
			NetNS: "/run/netns/beevm-5", ParentCgroup: "beevm/t7",
			Cgroups: []string{"memory.max=201326592", "pids.max=32", "cpu.max=100000 100000"}},
	}
}

func TestJailerArgs(t *testing.T) {
	c := jailedConfig(t)
	got := jailerArgs(c, "--api-sock", "/firecracker.sock")
	want := []string{
		"--id", "abc123", "--exec-file", c.FirecrackerBin, "--uid", "900005", "--gid", "900005",
		"--chroot-base-dir", c.Jail.ChrootBase, "--netns", "/run/netns/beevm-5",
		"--cgroup-version", "2", "--parent-cgroup", "beevm/t7",
		"--cgroup", "memory.max=201326592", "--cgroup", "pids.max=32", "--cgroup", "cpu.max=100000 100000",
		"--", "--api-sock", "/firecracker.sock",
	}
	if !slices.Equal(got, want) {
		t.Errorf("jailer args\n got %q\nwant %q", got, want)
	}
	// Security properties that must never change silently.
	for _, bad := range []string{"--no-seccomp", "--daemonize", "--new-pid-ns"} {
		if slices.Contains(got, bad) {
			t.Errorf("jailer args contain %s", bad)
		}
	}
}

func TestJailerArgsWithoutNetnsOrCgroup(t *testing.T) {
	c := jailedConfig(t)
	c.Jail.NetNS, c.Jail.ParentCgroup = "", ""
	got := strings.Join(jailerArgs(c), " ")
	for _, s := range []string{"--netns", "--cgroup", "--parent-cgroup"} {
		if strings.Contains(got, s) {
			t.Errorf("args %q contain %s", got, s)
		}
	}
}

func TestJailPaths(t *testing.T) {
	c := jailedConfig(t)
	root := filepath.Join(c.Jail.ChrootBase, "firecracker", "abc123", "root")
	if c.JailRoot() != root {
		t.Errorf("JailRoot = %s, want %s", c.JailRoot(), root)
	}
	if c.SocketPath() != filepath.Join(root, "firecracker.sock") || c.VsockPath() != filepath.Join(root, "v.sock") {
		t.Errorf("sockets not in the jail: %s %s", c.SocketPath(), c.VsockPath())
	}
	if c.ConsolePath() != filepath.Join(c.Dir(), "console.log") {
		t.Errorf("console log should stay in the VM folder: %s", c.ConsolePath())
	}
	if got := c.fcPath(c.MetricsPath()); got != "/metrics.json" {
		t.Errorf("fcPath(metrics) = %s", got)
	}
	if got := c.fcPath("/elsewhere/x"); got != "/elsewhere/x" {
		t.Errorf("fcPath of a path outside the jail = %s", got)
	}
	c.Jail = nil
	if c.JailRoot() != "" || c.SocketPath() != filepath.Join(c.Dir(), "firecracker.sock") {
		t.Error("without a jail the paths must stay as before")
	}
}

func TestJailValidate(t *testing.T) {
	c := jailedConfig(t)
	if err := c.Validate(); err != nil {
		t.Fatalf("valid config: %v", err)
	}
	c.Jail.UID = 0
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "UID") {
		t.Errorf("UID 0 (root) must be refused, got %v", err)
	}
	c = jailedConfig(t)
	c.SharedRootfs = false
	if err := c.Validate(); err == nil {
		t.Error("a jail without SharedRootfs must be refused")
	}
}

func TestSDKConfigInJail(t *testing.T) {
	c := jailedConfig(t)
	c.Net = &NetIface{HostDev: "tap0", GuestMAC: "06:00:AC:10:00:02"}
	v := newVM(c, 0)
	sc := v.sdkConfig()
	if sc.KernelImagePath != "/vmlinux" || *sc.Drives[0].PathOnHost != "/rootfs.ext4" {
		t.Errorf("paths must be as the jail sees them: %s %s", sc.KernelImagePath, *sc.Drives[0].PathOnHost)
	}
	if !sc.DisableValidation {
		t.Error("the SDK cannot check jail paths on the host")
	}
	if len(sc.NetworkInterfaces) != 1 || sc.NetworkInterfaces[0].StaticConfiguration.HostDevName != "tap0" {
		t.Errorf("network card missing: %+v", sc.NetworkInterfaces)
	}
	c.Jail, c.Net = nil, nil
	sc = newVM(c, 0).sdkConfig()
	if sc.KernelImagePath != c.KernelPath || sc.DisableValidation || len(sc.NetworkInterfaces) != 0 {
		t.Error("without isolation the SDK config must be as before")
	}
}

func TestWorldReadable(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := worldReadable(p); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o644 {
		t.Errorf("mode %v, want 0644", st.Mode().Perm())
	}
}

type fakeIsolator struct {
	prepared, released int
	err                error
}

func (f *fakeIsolator) Name() string { return "fake" }
func (f *fakeIsolator) Prepare(cfg *Config) (func(), error) {
	if f.err != nil {
		return nil, f.err
	}
	f.prepared++
	cfg.ExtraBootArgs += " isolated=1"
	return func() { f.released++ }, nil
}

func TestIsolatorPrepareAndReleaseOnce(t *testing.T) {
	c := jailedConfig(t)
	c.Jail = nil
	iso := &fakeIsolator{}
	c.Isolator = iso
	vm, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	if iso.prepared != 1 || !strings.Contains(vm.Config().BootArgs(), "isolated=1") {
		t.Fatalf("isolator not used: %+v %s", iso, vm.Config().BootArgs())
	}
	_ = vm.Cleanup(false)
	_ = vm.Cleanup(false)
	if iso.released != 1 {
		t.Errorf("released %d times, want once", iso.released)
	}
}

func TestIsolatorReleasedWhenNewFails(t *testing.T) {
	c := jailedConfig(t)
	c.Jail = nil
	c.KernelPath = "/does/not/exist"
	iso := &fakeIsolator{}
	c.Isolator = iso
	if _, err := New(c); err == nil {
		t.Fatal("New must fail")
	}
	if iso.prepared != 1 || iso.released != 1 {
		t.Errorf("prepare/release = %d/%d, want 1/1", iso.prepared, iso.released)
	}
}
