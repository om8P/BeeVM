package main

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeHome builds a BEEVM_HOME with the files the doctor looks for. skip names files to leave out.
func fakeHome(t *testing.T, skip ...string) string {
	t.Helper()
	home := t.TempDir()
	files := map[string]os.FileMode{
		"bin/firecracker":           0o755,
		"images/vmlinux":            0o644,
		"images/rootfs.ext4":        0o644,
		"images/rootfs-python.ext4": 0o644,
		"images/rootfs-node.ext4":   0o644,
	}
	for _, s := range skip {
		delete(files, s)
	}
	for name, mode := range files {
		p := filepath.Join(home, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("x"), mode)
	}
	return home
}

func baseEnv(t *testing.T, home string) doctorEnv {
	kvm := filepath.Join(t.TempDir(), "kvm")
	os.WriteFile(kvm, nil, 0o666) // stands in for /dev/kvm
	return doctorEnv{
		home: home, kvm: kvm, power: filepath.Join(t.TempDir(), "none"), proc: t.TempDir(),
		addr: "127.0.0.1:1", client: &http.Client{Timeout: time.Second},
	}
}

func find(checks []check, name string) check {
	for _, c := range checks {
		if c.Name == name {
			return c
		}
	}
	return check{}
}

func TestDoctorAllGood(t *testing.T) {
	home := fakeHome(t)
	os.MkdirAll(filepath.Join(home, "snapshots"), 0o755)
	os.WriteFile(filepath.Join(home, "beevm.db"), nil, 0o644)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	e := baseEnv(t, home)
	e.addr = strings.TrimPrefix(srv.URL, "http://")
	e.snapshots = func() ([]snapInfo, error) {
		return []snapInfo{{"binary", false, true}, {"python", false, true}, {"node", false, true}, {"python", true, true}}, nil
	}
	e.tenants = func() (int, error) { return 2, nil }
	ui := t.TempDir()
	os.WriteFile(filepath.Join(ui, "index.html"), nil, 0o644)
	e.uiDir = ui

	checks := doctorChecks(e)
	for _, c := range checks {
		if c.Level == levelFail || (c.Level == levelWarn && c.Name != "disk") {
			t.Errorf("unexpected %s %s: %s", c.Level, c.Name, c.Detail)
		}
	}
	if s := find(checks, "snapshots"); s.Level != levelOK || !strings.Contains(s.Detail, "function snapshots: 1") {
		t.Errorf("snapshots: %+v", s)
	}
	if find(checks, "server").Level != levelOK || find(checks, "dashboard").Level != levelOK {
		t.Errorf("server/dashboard: %+v", checks)
	}
	if _, ok := map[string]bool{"leftover VMs": true}[find(checks, "leftover VMs").Name]; ok {
		t.Error("leftover check must be skipped while the server runs")
	}
	var buf bytes.Buffer
	printChecks(&buf, checks)
	if !strings.Contains(buf.String(), "ready") || strings.Contains(buf.String(), "problem") {
		t.Errorf("report:\n%s", buf.String())
	}
}

func TestDoctorMissingThingsSayWhatToRun(t *testing.T) {
	home := fakeHome(t, "images/rootfs.ext4", "images/rootfs-node.ext4")
	e := baseEnv(t, home)
	e.snapshots = func() ([]snapInfo, error) { t.Error("snapshots checked without images"); return nil, nil }
	checks := doctorChecks(e)

	if c := find(checks, "image binary"); c.Level != levelFail || c.Fix != "make images" {
		t.Errorf("binary image: %+v", c)
	}
	if c := find(checks, "image node"); c.Level != levelWarn {
		t.Errorf("node image: %+v", c)
	}
	if c := find(checks, "server"); c.Level != levelInfo || !strings.Contains(c.Fix, "serve") {
		t.Errorf("server: %+v", c)
	}
	if c := find(checks, "dashboard"); c.Level != levelInfo || c.Fix != "make ui" {
		t.Errorf("dashboard: %+v", c)
	}
	var buf bytes.Buffer
	printChecks(&buf, checks)
	if !strings.Contains(buf.String(), "1 problem(s) to fix") || !strings.Contains(buf.String(), "-> make images") {
		t.Errorf("report:\n%s", buf.String())
	}
}

func TestDoctorNoHomeAndNoKVM(t *testing.T) {
	e := baseEnv(t, filepath.Join(t.TempDir(), "missing"))
	e.kvm = filepath.Join(t.TempDir(), "no-kvm")
	checks := doctorChecks(e)
	if c := find(checks, "kvm"); c.Level != levelFail {
		t.Errorf("kvm: %+v", c)
	}
	if c := find(checks, "home"); c.Level != levelFail || c.Fix != "make setup" {
		t.Errorf("home: %+v", c)
	}
	if find(checks, "image binary").Name != "" {
		t.Error("image checks must be skipped without a home")
	}
}

func TestCheckSnapshots(t *testing.T) {
	list := func(s ...snapInfo) func() ([]snapInfo, error) {
		return func() ([]snapInfo, error) { return s, nil }
	}
	all := []snapInfo{{"binary", false, true}, {"python", false, true}, {"node", false, true}}
	if c := checkSnapshots(list(all...), true); c.Level != levelOK {
		t.Errorf("all current: %+v", c)
	}
	if c := checkSnapshots(list(append(all, snapInfo{"binary", false, false})...), true); c.Level != levelWarn || !strings.Contains(c.Fix, "--stale") {
		t.Errorf("stale: %+v", c)
	}
	if c := checkSnapshots(list(all[0]), true); c.Level != levelWarn || !strings.Contains(c.Detail, "missing: python, node") {
		t.Errorf("missing while serving: %+v", c)
	}
	if c := checkSnapshots(list(), false); c.Level != levelInfo {
		t.Errorf("missing, no server: %+v", c)
	}
	if c := checkSnapshots(func() ([]snapInfo, error) { return nil, errors.New("boom") }, false); c.Level != levelWarn {
		t.Errorf("error: %+v", c)
	}
}

func TestCheckTenants(t *testing.T) {
	home := t.TempDir()
	if c := checkTenants(home, nil); c.Level != levelInfo {
		t.Errorf("no database: %+v", c)
	}
	os.WriteFile(filepath.Join(home, "beevm.db"), nil, 0o644)
	if c := checkTenants(home, func() (int, error) { return 0, nil }); c.Level != levelInfo || !strings.Contains(c.Fix, "tenant add") {
		t.Errorf("no tenants: %+v", c)
	}
	if c := checkTenants(home, func() (int, error) { return 3, nil }); c.Level != levelOK || c.Detail != "3" {
		t.Errorf("tenants: %+v", c)
	}
}

func TestCheckLeftoversAndPower(t *testing.T) {
	proc := t.TempDir()
	for pid, comm := range map[string]string{"10": "bash\n", "11": "firecracker\n", "12": "firecracker\n"} {
		os.MkdirAll(filepath.Join(proc, pid), 0o755)
		os.WriteFile(filepath.Join(proc, pid, "comm"), []byte(comm), 0o644)
	}
	if c, ok := checkLeftovers(proc); !ok || c.Level != levelWarn || !strings.Contains(c.Detail, "2 firecracker") {
		t.Errorf("leftovers: %+v", c)
	}

	power := t.TempDir()
	if _, ok := checkPower(power); ok {
		t.Error("no power supplies: nothing to report")
	}
	os.MkdirAll(filepath.Join(power, "BAT1"), 0o755) // a battery has no "online" file
	os.MkdirAll(filepath.Join(power, "ADP1"), 0o755)
	os.WriteFile(filepath.Join(power, "ADP1", "online"), []byte("0\n"), 0o644)
	if c, ok := checkPower(power); !ok || c.Level != levelWarn {
		t.Errorf("on battery: %+v", c)
	}
	os.WriteFile(filepath.Join(power, "ADP1", "online"), []byte("1\n"), 0o644)
	if c, ok := checkPower(power); !ok || c.Level != levelOK {
		t.Errorf("plugged in: %+v", c)
	}
}

func TestCheckBenchTools(t *testing.T) {
	cases := []struct {
		k6, mpl bool
		level   string
		fix     string
	}{
		{true, true, levelOK, ""},
		{false, true, levelInfo, "make bench-setup"},
		{true, false, levelInfo, "make bench-setup"},
		{false, false, levelInfo, "make bench-setup"},
	}
	for _, c := range cases {
		got := checkBenchTools(c.k6, c.mpl)
		if got.Level != c.level || got.Fix != c.fix {
			t.Errorf("k6=%v matplotlib=%v: got %+v", c.k6, c.mpl, got)
		}
	}
}
