package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/sys/unix"

	"github.com/om8P/BeeVM/internal/agentproto"
)

// Check results of `beevmd doctor`.
const (
	levelOK   = "ok"
	levelInfo = "info" // optional part missing, nothing broken
	levelWarn = "warn" // works, but slower or surprising
	levelFail = "FAIL" // BeeVM cannot work until this is fixed
)

// check is one line of the doctor's report.
type check struct {
	Level  string
	Name   string
	Detail string
	Fix    string // what to run or do; empty when nothing is needed
}

// doctorEnv is everything the checks look at, so tests can point them at temporary folders.
type doctorEnv struct {
	home    string // BEEVM_HOME
	kvm     string // /dev/kvm
	power   string // /sys/class/power_supply
	proc    string // /proc
	addr    string // where `serve` listens
	uiDir   string // dashboard folder, already resolved
	minFree uint64 // warn below this many free bytes in home
	client  *http.Client
	// snapshots lists the snapshots on disk; nil = skip the check.
	snapshots func() ([]snapInfo, error)
	tenants   func() (int, error)
	// benchTools says whether k6 and python3-matplotlib are installed (make bench); nil = skip.
	benchTools func() (k6, matplotlib bool)
}

type snapInfo struct {
	runtime  string
	function bool
	current  bool
}

// runDoctor checks the setup and says what to fix. Exit code 1 when anything FAILs.
func runDoctor(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:8080", "where beevmd serve listens")
	uiDir := fs.String("ui-dir", "web/dist", "dashboard folder (as for serve)")
	fs.Parse(args)

	home := beevmHome()
	env := doctorEnv{
		home: home, kvm: "/dev/kvm", power: "/sys/class/power_supply", proc: "/proc",
		addr: *addr, uiDir: findUIDir(*uiDir, os.Executable), minFree: 2 << 30,
		client: &http.Client{Timeout: 2 * time.Second},
		snapshots: func() ([]snapInfo, error) {
			m, err := snapshotManager(home, nil, func(string, ...any) {})
			if err != nil {
				return nil, err
			}
			list, err := m.List()
			if err != nil {
				return nil, err
			}
			out := make([]snapInfo, 0, len(list))
			for _, s := range list {
				out = append(out, snapInfo{runtime: s.RuntimeName(), function: s.IsFunction(), current: s.Current})
			}
			return out, nil
		},
		benchTools: benchToolsInstalled,
		tenants: func() (int, error) {
			reg, err := openRegistry(home)
			if err != nil {
				return 0, err
			}
			defer reg.Close()
			ts, err := reg.ListTenants()
			return len(ts), err
		},
	}
	checks := doctorChecks(env)
	printChecks(os.Stdout, checks)
	for _, c := range checks {
		if c.Level == levelFail {
			return 1
		}
	}
	return 0
}

// doctorChecks runs every check in order. Checks are skipped when what they need is missing
// (no snapshot check without images), so the first FAIL is the one to fix first.
func doctorChecks(e doctorEnv) []check {
	var out []check
	add := func(c check) { out = append(out, c) }

	add(checkKVM(e.kvm))

	homeOK := dirExists(e.home)
	if homeOK {
		add(check{levelOK, "home", e.home, ""})
	} else {
		add(check{levelFail, "home", e.home + " does not exist", "make setup"})
	}

	imagesOK := homeOK
	if homeOK {
		for _, c := range checkFiles(e.home) {
			if c.Level == levelFail {
				imagesOK = false
			}
			add(c)
		}
		add(checkDisk(e.home, e.minFree))
	}

	server := checkServer(e.client, e.addr)
	add(server)
	serverUp := server.Level == levelOK

	switch {
	case !imagesOK:
	case e.snapshots != nil && dirExists(filepath.Join(e.home, "snapshots")):
		add(checkSnapshots(e.snapshots, serverUp))
	default:
		add(check{levelInfo, "snapshots", "none yet (serve builds them at start)", ""})
	}
	if homeOK && e.tenants != nil {
		add(checkTenants(e.home, e.tenants))
	}
	if !serverUp {
		if c, ok := checkLeftovers(e.proc); ok {
			add(c)
		}
	}
	add(checkUI(e.uiDir))
	if e.benchTools != nil {
		add(checkBenchTools(e.benchTools()))
	}
	if c, ok := checkPower(e.power); ok {
		add(c)
	}
	return out
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func checkKVM(path string) check {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	switch {
	case err == nil:
		f.Close()
		return check{levelOK, "kvm", path + " usable", ""}
	case errors.Is(err, fs.ErrNotExist):
		return check{levelFail, "kvm", path + " does not exist: no hardware virtualization here",
			"enable virtualization in the BIOS; on Windows use WSL2 with nestedVirtualization=true"}
	case errors.Is(err, fs.ErrPermission):
		return check{levelFail, "kvm", "no permission to open " + path,
			"make setup adds you to the kvm group; then log in again (WSL: wsl --shutdown in PowerShell) and check with: id"}
	}
	return check{levelFail, "kvm", err.Error(), ""}
}

// checkFiles looks for Firecracker, the kernel and the root filesystems.
func checkFiles(home string) []check {
	var out []check
	fc := filepath.Join(home, "bin", "firecracker")
	if st, err := os.Stat(fc); err != nil || st.Mode()&0o111 == 0 {
		out = append(out, check{levelFail, "firecracker", fc + " missing", "make setup"})
	} else {
		version := "installed"
		if b, err := exec.Command(fc, "--version").Output(); err == nil {
			version = strings.TrimSpace(strings.SplitN(string(b), "\n", 2)[0])
		}
		out = append(out, check{levelOK, "firecracker", version, ""})
	}
	images := []struct {
		name, file, level string
	}{
		{"kernel", "vmlinux", levelFail},
		{"image binary", "rootfs.ext4", levelFail},
		{"image python", "rootfs-python.ext4", levelWarn},
		{"image node", "rootfs-node.ext4", levelWarn},
	}
	for _, im := range images {
		p := filepath.Join(home, "images", im.file)
		if st, err := os.Stat(p); err != nil || st.Size() == 0 {
			detail := p + " missing"
			if im.level == levelWarn {
				detail += " (functions of this runtime cannot run)"
			}
			out = append(out, check{im.level, im.name, detail, "make images"})
		} else {
			out = append(out, check{levelOK, im.name, fmt.Sprintf("%s (%s)", im.file, humanBytes(st.Size())), ""})
		}
	}
	return out
}

func checkDisk(home string, minFree uint64) check {
	var st unix.Statfs_t
	if err := unix.Statfs(home, &st); err != nil {
		return check{levelWarn, "disk", err.Error(), ""}
	}
	free := st.Bavail * uint64(st.Bsize)
	if free < minFree {
		return check{levelWarn, "disk", humanBytes(int64(free)) + " free in " + home + " (each snapshot takes about as much as its VM's memory)",
			"free some space, or remove old snapshots: beevmd snapshot rm --stale"}
	}
	return check{levelOK, "disk", humanBytes(int64(free)) + " free", ""}
}

func checkServer(c *http.Client, addr string) check {
	url := "http://" + addr + "/healthz"
	resp, err := c.Get(url)
	if err != nil {
		return check{levelInfo, "server", "not running on " + addr, "./bin/beevmd serve"}
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return check{levelWarn, "server", fmt.Sprintf("%s answered %d", url, resp.StatusCode), ""}
	}
	return check{levelOK, "server", "running on " + addr, ""}
}

// checkSnapshots wants a current runtime snapshot per runtime; without them every new VM
// boots (about a second) instead of being restored.
func checkSnapshots(list func() ([]snapInfo, error), serverUp bool) check {
	snaps, err := list()
	if err != nil {
		return check{levelWarn, "snapshots", err.Error(), "./bin/beevmd snapshot ls"}
	}
	current := map[string]bool{}
	stale, functions := 0, 0
	for _, s := range snaps {
		switch {
		case !s.current:
			stale++
		case s.function:
			functions++
		default:
			current[s.runtime] = true
		}
	}
	var have, missing []string
	for _, rt := range []string{agentproto.RuntimeBinary, agentproto.RuntimePython, agentproto.RuntimeNode} {
		if current[rt] {
			have = append(have, rt)
		} else {
			missing = append(missing, rt)
		}
	}
	detail := fmt.Sprintf("runtime: %s; function snapshots: %d", listOrNone(have), functions)
	if stale > 0 {
		detail += fmt.Sprintf("; stale: %d", stale)
	}
	switch {
	case len(missing) > 0 && !serverUp:
		return check{levelInfo, "snapshots", detail + " (serve builds the missing ones at start)", ""}
	case len(missing) > 0:
		return check{levelWarn, "snapshots", detail + "; missing: " + strings.Join(missing, ", ") + " (new VMs of these runtimes boot, about 1 s)",
			"read the server log, or: ./bin/beevmd snapshot build --runtime " + missing[0]}
	case stale > 0:
		return check{levelWarn, "snapshots", detail, "./bin/beevmd snapshot rm --stale"}
	}
	return check{levelOK, "snapshots", detail, ""}
}

func listOrNone(s []string) string {
	if len(s) == 0 {
		return "none"
	}
	return strings.Join(s, ", ")
}

func checkTenants(home string, count func() (int, error)) check {
	if _, err := os.Stat(filepath.Join(home, "beevm.db")); err != nil {
		return check{levelInfo, "tenants", "none yet", "./bin/beevmd tenant add <name>"}
	}
	n, err := count()
	if err != nil {
		return check{levelWarn, "tenants", err.Error(), ""}
	}
	if n == 0 {
		return check{levelInfo, "tenants", "none yet", "./bin/beevmd tenant add <name>"}
	}
	return check{levelOK, "tenants", fmt.Sprintf("%d", n), ""}
}

// checkLeftovers finds Firecracker processes while no server runs: VMs of a crashed server.
func checkLeftovers(proc string) (check, bool) {
	entries, err := os.ReadDir(proc)
	if err != nil {
		return check{}, false
	}
	var pids []string
	for _, e := range entries {
		if b, err := os.ReadFile(filepath.Join(proc, e.Name(), "comm")); err == nil && strings.TrimSpace(string(b)) == "firecracker" {
			pids = append(pids, e.Name())
		}
	}
	if len(pids) == 0 {
		return check{levelOK, "leftover VMs", "none", ""}, true
	}
	return check{levelWarn, "leftover VMs", fmt.Sprintf("%d firecracker processes but no server (pids %s)", len(pids), strings.Join(pids, " ")),
		"look at them with pgrep -a firecracker, then kill <pid>; their folders are in ~/.beevm/run/serve/"}, true
}

func checkUI(dir string) check {
	if dir != "" && hasIndex(dir) {
		return check{levelOK, "dashboard", "built (" + dir + ")", ""}
	}
	return check{levelInfo, "dashboard", "not built (optional)", "make ui"}
}

// checkPower warns when the machine runs on battery: benchmarks are then about 2x slower.
// Machines without power-supply information (most servers) have nothing to report.
func checkPower(dir string) (check, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return check{}, false
	}
	sawAC := false
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name(), "online"))
		if err != nil {
			continue
		}
		sawAC = true
		if strings.TrimSpace(string(b)) == "1" {
			return check{levelOK, "power", "plugged in (still use a performance power mode for benchmarks)", ""}, true
		}
	}
	if !sawAC {
		return check{}, false
	}
	return check{levelWarn, "power", "on battery: everything is about 2x slower", "plug in before benchmarking"}, true
}

func printChecks(w io.Writer, checks []check) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fails, warns := 0, 0
	for _, c := range checks {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", c.Level, c.Name, c.Detail)
		if c.Fix != "" && c.Level != levelOK {
			fmt.Fprintf(tw, "\t\t-> %s\n", c.Fix)
		}
		switch c.Level {
		case levelFail:
			fails++
		case levelWarn:
			warns++
		}
	}
	tw.Flush()
	switch {
	case fails > 0:
		fmt.Fprintf(w, "\n%d problem(s) to fix (FAIL), %d warning(s)\n", fails, warns)
	case warns > 0:
		fmt.Fprintf(w, "\nready, with %d warning(s)\n", warns)
	default:
		fmt.Fprintln(w, "\nready")
	}
}

// checkBenchTools reports the tools `make bench` needs besides the Go binaries.
func checkBenchTools(k6, matplotlib bool) check {
	switch {
	case k6 && matplotlib:
		return check{levelOK, "bench tools", "k6 and matplotlib installed", ""}
	case !k6 && !matplotlib:
		return check{levelInfo, "bench tools", "k6 and python3-matplotlib missing (only needed for make bench)", "make bench-setup"}
	case !k6:
		return check{levelInfo, "bench tools", "k6 missing (only needed for make bench)", "make bench-setup"}
	}
	return check{levelInfo, "bench tools", "python3-matplotlib missing (only needed for make bench)", "make bench-setup"}
}

// benchToolsInstalled looks for k6 on PATH and tries to import matplotlib.
func benchToolsInstalled() (k6, matplotlib bool) {
	_, err := exec.LookPath("k6")
	k6 = err == nil
	matplotlib = exec.Command(envOr("BEEVM_PYTHON", "/usr/bin/python3"), "-c", "import matplotlib").Run() == nil
	return
}
