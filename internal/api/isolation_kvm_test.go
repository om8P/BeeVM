//go:build kvm

package api

// Milestone 9: attacks against a real isolated server (jailer + cgroups + per-VM network),
// two tenants (alice, bob), the real warm pool. Needs root: run `make test-isolation`.
// At the end it writes bench/results/isolation-<time>.md, the results table for the report.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/om8P/BeeVM/internal/apiclient"
	"github.com/om8P/BeeVM/internal/apitypes"
	"github.com/om8P/BeeVM/internal/codestore"
	"github.com/om8P/BeeVM/internal/isolation"
	"github.com/om8P/BeeVM/internal/netns"
	"github.com/om8P/BeeVM/internal/pool"
	"github.com/om8P/BeeVM/internal/registry"
	"github.com/om8P/BeeVM/internal/runner"
	"github.com/om8P/BeeVM/internal/snapshot"
	"github.com/om8P/BeeVM/internal/vmm"
)

// isoRow is one line of the results table.
type isoRow struct{ Attack, Mechanism, Expected, Observed, Pass string }

var (
	isoRowsMu sync.Mutex
	isoRows   []isoRow
)

func report(t *testing.T, attack, mechanism, expected, observed string) {
	t.Helper()
	pass := "yes"
	if t.Failed() {
		pass = "**NO**"
	}
	isoRowsMu.Lock()
	isoRows = append(isoRows, isoRow{attack, mechanism, expected, observed, pass})
	isoRowsMu.Unlock()
	t.Logf("%s: %s", attack, observed)
}

func writeIsolationReport(t *testing.T) {
	isoRowsMu.Lock()
	defer isoRowsMu.Unlock()
	if len(isoRows) == 0 {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Isolation results (%s)\n\n", time.Now().Format("2006-01-02 15:04"))
	b.WriteString("Server: `--isolation jailer` (jailer chroot + own UID, cgroups v2, own network namespace per VM, ")
	b.WriteString("nftables between tenants), warm pool, two tenants (alice = attacker, bob = neighbour), ")
	b.WriteString("1 vCPU / 128 MiB VMs, WSL2. Produced by `make test-isolation` (internal/api/isolation_kvm_test.go).\n\n")
	b.WriteString("| Attack | Stopped by | Expected | Observed | Pass |\n|---|---|---|---|---|\n")
	for _, r := range isoRows {
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n", r.Attack, r.Mechanism, r.Expected, r.Observed, r.Pass)
	}
	dir := "../../bench/results"
	_ = os.MkdirAll(dir, 0o755)
	p := filepath.Join(dir, "isolation-"+time.Now().Format("20060102-150405")+".md")
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		t.Logf("could not write report: %v", err)
		return
	}
	t.Logf("results table: %s\n%s", p, b.String())
}

// isoEnv is an isolated server with two tenants.
type isoEnv struct {
	alice, bob     *apiclient.Client
	aliceID, bobID int64
	iso            *isolation.Manager
	w              *runner.WarmRunner
	home, work     string
}

func realHome() string {
	if h := os.Getenv("BEEVM_HOME"); h != "" {
		return h
	}
	if su := os.Getenv("SUDO_USER"); su != "" {
		return filepath.Join("/home", su, ".beevm")
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".beevm")
}

// prebuilt returns bin/functions/<name> (built by `make examples`; there is no Go under sudo).
func prebuilt(t *testing.T, name string) string {
	t.Helper()
	p, _ := filepath.Abs("../../bin/functions/" + name)
	if _, err := os.Stat(p); err != nil {
		t.Skipf("skipping, %v (run: make examples)", err)
	}
	return p
}

// isoServer starts an isolated API server. memOverhead 0 = default.
func isoServer(t *testing.T, cpuPercent, memOverheadMiB int) *isoEnv {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root: run make test-isolation")
	}
	home := realHome()
	base := vmm.Config{
		FirecrackerBin: filepath.Join(home, "bin", "firecracker"),
		KernelPath:     filepath.Join(home, "images", "vmlinux"),
		RootfsPath:     filepath.Join(home, "images", "rootfs.ext4"),
	}
	for _, p := range []string{"/dev/kvm", base.FirecrackerBin, base.KernelPath, base.RootfsPath, filepath.Join(home, "bin", "jailer")} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("skipping, %v", err)
		}
	}
	work, err := os.MkdirTemp(filepath.Join(home, "run"), "isoe2e-")
	if err != nil {
		t.Fatal(err)
	}
	base.BaseDir = filepath.Join(work, "vms")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	iso, err := isolation.New(isolation.Config{Home: home, CPUPercent: cpuPercent, MemOverheadMiB: memOverheadMiB,
		Network: true, NetPrealloc: 4, LockPath: filepath.Join(work, "lock"), Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	base.Isolator = iso
	snaps, err := snapshot.New(filepath.Join(work, "snapshots"), base, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	w := runner.NewWarmRunner(base, snaps, pool.Config{MaxVMs: 6, KeepAlive: 5 * time.Minute, QueueWait: 60 * time.Second}, logger)
	reg, err := registry.Open(filepath.Join(work, "iso.db"))
	if err != nil {
		t.Fatal(err)
	}
	code, _ := codestore.New(filepath.Join(work, "functions"))
	ts := httptest.NewServer(New(Deps{Registry: reg, Code: code, Logger: logger, Runner: w}).Handler())

	env := &isoEnv{iso: iso, w: w, home: home, work: work}
	for _, name := range []string{"alice", "bob"} {
		tn, _ := reg.CreateTenant(name)
		key, _, _ := reg.CreateKey(tn.ID)
		if name == "alice" {
			env.alice, env.aliceID = apiclient.New(ts.URL, key), tn.ID
		} else {
			env.bob, env.bobID = apiclient.New(ts.URL, key), tn.ID
		}
	}
	t.Cleanup(func() {
		ts.Close()
		_ = w.Close()
		reg.Close()
		_ = iso.Close()
		assertHostClean(t, home)
		_ = os.RemoveAll(work)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := snaps.Ensure(ctx, 128, 1); err != nil {
		t.Fatal(err)
	}
	return env
}

// assertHostClean checks that the server left nothing on the host.
func assertHostClean(t *testing.T, home string) {
	t.Helper()
	if entries, _ := os.ReadDir("/run/netns"); len(entries) > 0 {
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "beevm-") {
				t.Errorf("network namespace %s left", e.Name())
			}
		}
	}
	ifs, _ := net.Interfaces()
	for _, i := range ifs {
		if strings.HasPrefix(i.Name, "bv") {
			t.Errorf("host veth %s left", i.Name)
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(home, "jail", "firecracker")); len(entries) > 0 {
		t.Errorf("%d jails left", len(entries))
	}
	tenants, _ := os.ReadDir("/sys/fs/cgroup/beevm")
	for _, tn := range tenants {
		if !tn.IsDir() {
			continue
		}
		leaves, _ := os.ReadDir(filepath.Join("/sys/fs/cgroup/beevm", tn.Name()))
		for _, l := range leaves {
			if l.IsDir() {
				t.Errorf("cgroup beevm/%s/%s left", tn.Name(), l.Name())
			}
		}
	}
	if out, _ := exec.Command("pgrep", "-f", filepath.Join(home, "jail")).Output(); strings.TrimSpace(string(out)) != "" {
		t.Errorf("jailed processes left: %s", out)
	}
}

func deployAs(t *testing.T, c *apiclient.Client, name, fn string, timeoutMS int64) {
	t.Helper()
	deployFile(t, c, name, prebuilt(t, fn), apitypes.FunctionConfig{MemoryMiB: 128, VCPUs: 1, TimeoutMS: timeoutMS})
}

func invokeJSON(t *testing.T, c *apiclient.Client, name, payload string) (map[string]any, apiclient.InvokeResult, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	res, err := c.Invoke(ctx, name, []byte(payload))
	if err != nil {
		return nil, res, err
	}
	var out map[string]any
	if err := json.Unmarshal(res.Output, &out); err != nil {
		t.Fatalf("%s: output %s: %v", name, res.Output, err)
	}
	return out, res, nil
}

// vmOf returns the VM (ID and address) running function name for this client.
func vmOf(t *testing.T, c *apiclient.Client, name string) apitypes.VM {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		vms, err := c.VMs(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range vms {
			if v.Function == name && v.State != "restoring" {
				return v
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no VM for %s: %+v", name, vms)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// jailedPID reads the firecracker PID the jailer wrote into the VM's jail.
func jailedPID(t *testing.T, home, vmID string) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, "jail", "firecracker", vmID, "root", "firecracker.pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

func cgroupFile(t *testing.T, tenantID int64, vmID, file string) string {
	t.Helper()
	b, err := os.ReadFile(fmt.Sprintf("/sys/fs/cgroup/beevm/t%d/%s/%s", tenantID, vmID, file))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

// cgroupStat reads one "key value" line of a cgroup stat file.
func cgroupStat(t *testing.T, tenantID int64, vmID, file, key string) int64 {
	t.Helper()
	for _, line := range strings.Split(cgroupFile(t, tenantID, vmID, file), "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[0] == key {
			n, _ := strconv.ParseInt(f[1], 10, 64)
			return n
		}
	}
	t.Fatalf("%s has no %s", file, key)
	return 0
}

// neighbourLatency calls bob's echo function n times and returns the median round trip.
func neighbourLatency(t *testing.T, c *apiclient.Client, n int) time.Duration {
	t.Helper()
	var ds []time.Duration
	for i := 0; i < n; i++ {
		t0 := time.Now()
		if _, _, err := invokeJSON(t, c, "echo", `{"mode":"echo"}`); err != nil {
			t.Errorf("neighbour call failed: %v", err)
			continue
		}
		ds = append(ds, time.Since(t0))
		time.Sleep(20 * time.Millisecond)
	}
	if len(ds) == 0 {
		return 0
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	return ds[len(ds)/2]
}

// neighbourOK checks that the neighbour's median during an attack stays within 5x of before
// (with a 25 ms floor: single-digit milliseconds jitter a lot under WSL2).
func neighbourOK(t *testing.T, before, during time.Duration) bool {
	limit := 5 * before
	if limit < 25*time.Millisecond {
		limit = 25 * time.Millisecond
	}
	if during > limit {
		t.Errorf("neighbour slowed down: median %v during the attack, %v before", during, before)
		return false
	}
	return true
}

func ms(d time.Duration) string { return fmt.Sprintf("%.1f ms", float64(d.Microseconds())/1000) }

func TestIsolation(t *testing.T) {
	t.Cleanup(func() { writeIsolationReport(t) })

	t.Run("main", func(t *testing.T) {
		env := isoServer(t, 100, 0)
		deployAs(t, env.bob, "echo", "testfn", 10000)
		if _, _, err := invokeJSON(t, env.bob, "echo", `{"mode":"echo"}`); err != nil {
			t.Fatal(err)
		}
		t.Run("hostread", func(t *testing.T) { testHostRead(t, env) })
		t.Run("network", func(t *testing.T) { testNetwork(t, env) })
		t.Run("forkbomb", func(t *testing.T) { testForkBomb(t, env) })
		t.Run("memhog", func(t *testing.T) { testMemHog(t, env) })
	})
	t.Run("cpuhog", func(t *testing.T) {
		env := isoServer(t, 50, 0)
		testCPUHog(t, env)
	})
	t.Run("memory.max", func(t *testing.T) {
		// memory.max below the guest's RAM: the host's limit must stop Firecracker itself.
		env := isoServer(t, 100, -64)
		testHostMemoryLimit(t, env)
	})
}

func testHostRead(t *testing.T, env *isoEnv) {
	canary := fmt.Sprintf("BEEVM-CANARY-%d", time.Now().UnixNano())
	hostFiles := []string{filepath.Join(env.work, "canary.txt"), "/tmp/beevm-canary.txt", filepath.Join(env.home, "canary.txt")}
	for _, p := range hostFiles {
		if err := os.WriteFile(p, []byte("secret: "+canary+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(p)
	}
	deployAs(t, env.alice, "hostread", "hostread", 30000)
	out, _, err := invokeJSON(t, env.alice, "hostread", fmt.Sprintf(`{"canary":%q}`, canary))
	if err != nil {
		t.Fatal(err)
	}
	found, _ := out["found"].([]any)
	if len(found) > 0 {
		t.Errorf("guest found the host's canary in %v", found)
	}
	if uid, _ := out["uid"].(float64); uid != 1000 {
		t.Errorf("function runs as uid %v, want 1000", out["uid"])
	}
	checks, _ := out["checks"].(map[string]any)
	for name, v := range checks {
		if s, _ := v.(string); !strings.HasPrefix(s, "denied") {
			t.Errorf("guest check %q: %v", name, v)
		}
	}
	// From the host's side: the VM's firecracker is jailed.
	vm := vmOf(t, env.alice, "hostread")
	pid := jailedPID(t, env.home, vm.ID)
	st, _ := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	status := string(st)
	for _, want := range []string{"Seccomp:\t2", "NoNewPrivs:\t1", "CapEff:\t0000000000000000"} {
		if !strings.Contains(status, want) {
			t.Errorf("firecracker %d: %q missing", pid, want)
		}
	}
	var jail []string
	if entries, err := os.ReadDir(fmt.Sprintf("/proc/%d/root", pid)); err == nil {
		for _, e := range entries {
			jail = append(jail, e.Name())
		}
	}
	var uid string
	for _, line := range strings.Split(status, "\n") {
		if strings.HasPrefix(line, "Uid:") {
			uid = strings.Fields(line)[1]
		}
	}
	report(t, "Host file read (search the guest for a canary written on the host; read /etc/shadow, write /dev/vda, replace the agent's files)",
		"MicroVM (own kernel and disk); guest user fn; host side: jailer chroot, own UID, seccomp",
		"canary not found; all tricks denied",
		fmt.Sprintf("%v files scanned, canary found %d times; %d/%d tricks denied; function uid %v; firecracker uid %s, seccomp 2, no capabilities, its / holds only %v",
			out["scanned"], len(found), countDenied(checks), len(checks), out["uid"], uid, jail))
}

func countDenied(checks map[string]any) int {
	n := 0
	for _, v := range checks {
		if s, _ := v.(string); strings.HasPrefix(s, "denied") {
			n++
		}
	}
	return n
}

func testNetwork(t *testing.T, env *isoEnv) {
	for _, c := range []*apiclient.Client{env.alice, env.bob} {
		deployAs(t, c, "listener", "netprobe", 30000)
		deployAs(t, c, "probe", "netprobe", 30000)
	}
	// The probe VMs exist first (we need alice's own address).
	for _, c := range []*apiclient.Client{env.alice, env.bob} {
		if _, _, err := invokeJSON(t, c, "probe", `{"mode":"info"}`); err != nil {
			t.Fatal(err)
		}
	}
	aliceProbe := vmOf(t, env.alice, "probe")

	// Listeners on port 8080 in alice's and bob's other VMs, for 8 s.
	var wg sync.WaitGroup
	accepted := map[string]float64{}
	var mu sync.Mutex
	for name, c := range map[string]*apiclient.Client{"alice": env.alice, "bob": env.bob} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, _, err := invokeJSON(t, c, "listener", `{"mode":"listen","port":8080,"ms":8000}`)
			if err != nil {
				t.Errorf("%s listener: %v", name, err)
				return
			}
			mu.Lock()
			accepted[name], _ = out["accepted"].(float64)
			mu.Unlock()
		}()
	}
	aliceL := vmOf(t, env.alice, "listener")
	bobL := vmOf(t, env.bob, "listener")
	time.Sleep(300 * time.Millisecond) // let the listeners start

	// A TCP server on the host itself (all addresses), which no VM may reach.
	hostLn, err := net.Listen("tcp", ":18080")
	if err != nil {
		t.Fatal(err)
	}
	defer hostLn.Close()
	go func() {
		for {
			c, err := hostLn.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	gateway := netip.MustParseAddr(aliceProbe.Addr).Prev().String() // the host's end of alice's veth
	hostIP := hostAddress(t)

	targets := map[string]string{
		"same tenant (alice -> alice)": aliceL.Addr + ":8080",
		"other tenant (alice -> bob)":  bobL.Addr + ":8080",
		"host via the VM's gateway":    gateway + ":18080",
		"host's own address":           hostIP + ":18080",
	}
	var list []string
	for _, v := range targets {
		list = append(list, fmt.Sprintf("%q", v))
	}
	out, _, err := invokeJSON(t, env.alice, "probe", fmt.Sprintf(`{"mode":"connect","timeout_ms":1000,"targets":[%s]}`, strings.Join(list, ",")))
	if err != nil {
		t.Fatal(err)
	}
	results, _ := out["results"].(map[string]any)
	var observed []string
	for label, target := range targets {
		r, _ := results[target].(string)
		ok := r == "ok"
		wantOK := strings.HasPrefix(label, "same tenant")
		if ok != wantOK {
			t.Errorf("%s (%s): %s", label, target, r)
		}
		state := "blocked"
		if ok {
			state = "connected"
		}
		observed = append(observed, label+": "+state)
	}
	sort.Strings(observed)

	// bob tries alice (the other direction).
	bout, _, err := invokeJSON(t, env.bob, "probe", fmt.Sprintf(`{"mode":"connect","timeout_ms":1000,"targets":[%q]}`, aliceL.Addr+":8080"))
	if err != nil {
		t.Fatal(err)
	}
	if r := bout["results"].(map[string]any)[aliceL.Addr+":8080"]; r == "ok" {
		t.Error("bob reached alice's VM")
	} else {
		observed = append(observed, "other tenant (bob -> alice): blocked")
	}

	// alice scans the whole first /24 of VM addresses.
	sout, _, err := invokeJSON(t, env.alice, "probe", `{"mode":"scan","cidr":"10.200.0.0/24","port":8080,"timeout_ms":500}`)
	if err != nil {
		t.Fatal(err)
	}
	open, _ := sout["open"].([]any)
	if len(open) != 1 || open[0] != aliceL.Addr {
		t.Errorf("scan found %v, want only alice's listener %s", open, aliceL.Addr)
	}
	observed = append(observed, fmt.Sprintf("scan of 10.200.0.0/24 finds %d VM(s) (own listener %s)", len(open), aliceL.Addr))
	wg.Wait()

	// The firewall's view: alice's set holds only alice's VMs.
	members, err := env.iso.Firewall().Members(env.aliceID)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range members {
		if m.String() == bobL.Addr {
			t.Errorf("bob's VM %s in alice's set", m)
		}
	}
	report(t, "Cross-tenant network probe (connect to another tenant's VM, to the host, scan the VM range)",
		"one network namespace per VM; nftables: per-tenant address sets, VM->host input dropped",
		"same tenant reachable (control); other tenant and host blocked; scan finds only own VMs",
		strings.Join(observed, "; "))
}

// hostAddress is the host's first non-loopback IPv4 address outside the VM range.
func hostAddress(t *testing.T) string {
	t.Helper()
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		p, err := netip.ParsePrefix(a.String())
		if err != nil || !p.Addr().Is4() || p.Addr().IsLoopback() || netns.ClonePrefix.Contains(p.Addr()) {
			continue
		}
		return p.Addr().String()
	}
	t.Skip("no host IPv4 address")
	return ""
}

func testForkBomb(t *testing.T, env *isoEnv) {
	deployAs(t, env.alice, "forkbomb", "forkbomb", 10000)
	if _, _, err := invokeJSON(t, env.alice, "forkbomb", `{"mode":"count"}`); err != nil { // VM ready
		t.Fatal(err)
	}
	before := neighbourLatency(t, env.bob, 15)

	var out map[string]any
	var bombErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		out, _, bombErr = invokeJSON(t, env.alice, "forkbomb", `{"ms":4000,"escape":true}`)
	}()
	time.Sleep(500 * time.Millisecond)
	vm := vmOf(t, env.alice, "forkbomb")
	during := neighbourLatency(t, env.bob, 15)
	pids := cgroupFile(t, env.aliceID, vm.ID, "pids.current")
	<-done
	if bombErr != nil {
		t.Fatalf("fork bomb call: %v", bombErr)
	}
	failed, _ := out["failed"].(float64)
	maxProcs, _ := out["max_user_procs"].(float64)
	if failed == 0 {
		t.Errorf("the guest's process limit never stopped the bomb: %v", out)
	}
	if maxProcs > 256 {
		t.Errorf("more than 256 processes: %v", out)
	}
	// The same VM again: everything the bomb left behind (even escaped with setsid) is gone.
	after, res, err := invokeJSON(t, env.alice, "forkbomb", `{"mode":"count"}`)
	if err != nil {
		t.Fatal(err)
	}
	if res.StartKind != "warm" {
		t.Errorf("the VM should survive the bomb and be reused, got start %q", res.StartKind)
	}
	if n, _ := after["user_procs"].(float64); n != 1 {
		t.Errorf("%v of the function user's processes left after the call (want only the counter itself)", n)
	}
	if p, _ := strconv.Atoi(pids); p > 32 {
		t.Errorf("firecracker cgroup has %d tasks (pids.max 32)", p)
	}
	ok := neighbourOK(t, before, during)
	report(t, "Fork bomb (copies of itself, escaping the process group with setsid)",
		"guest: user fn limited to 256 processes, agent kills leftovers after each call; host: pids.max=32 for Firecracker, VM boundary",
		"bomb hits the limit; VM and agent survive; nothing left; neighbour unaffected",
		fmt.Sprintf("%v started, %v refused (%v), at most %v processes; afterwards %v left; firecracker tasks %s; neighbour median %s before, %s during (ok=%v)",
			out["spawned"], out["failed"], out["first_error"], out["max_user_procs"], after["user_procs"], pids, ms(before), ms(during), ok))
}

func testMemHog(t *testing.T, env *isoEnv) {
	deployAs(t, env.alice, "memhog", "memhog", 30000)
	if _, _, err := invokeJSON(t, env.alice, "memhog", `{"mib":16}`); err != nil {
		t.Fatal(err)
	}
	vm := vmOf(t, env.alice, "memhog")
	before := neighbourLatency(t, env.bob, 10)
	_, _, err := invokeJSON(t, env.alice, "memhog", `{"mib":1024}`)
	var ae *apiclient.APIError
	if !errors.As(err, &ae) || ae.Status != 502 || !strings.Contains(ae.Error(), "killed") {
		t.Errorf("want 502 'function was killed', got %v", err)
	}
	during := neighbourLatency(t, env.bob, 10)
	_, res, err := invokeJSON(t, env.alice, "memhog", `{"mib":16}`)
	if err != nil {
		t.Fatalf("the VM should still work after its function was OOM-killed: %v", err)
	}
	if res.StartKind != "warm" || res.VMID != vm.ID {
		t.Errorf("want the same warm VM %s, got %s (%s)", vm.ID, res.VMID, res.StartKind)
	}
	peak := cgroupFile(t, env.aliceID, vm.ID, "memory.peak")
	max := cgroupFile(t, env.aliceID, vm.ID, "memory.max")
	hostKills := cgroupStat(t, env.aliceID, vm.ID, "memory.events", "oom_kill")
	if p, _ := strconv.ParseInt(peak, 10, 64); max != "max" {
		if m, _ := strconv.ParseInt(max, 10, 64); p > m {
			t.Errorf("memory.peak %d > memory.max %d", p, m)
		}
	}
	if hostKills != 0 {
		t.Errorf("the host had to OOM-kill firecracker %d times; the guest should handle it", hostKills)
	}
	errText := ""
	if ae != nil {
		errText = ae.Message
	}
	report(t, "Memory hog (allocate and touch 1 GiB in a 128 MiB VM)",
		"guest RAM size; guest OOM killer prefers the function (agent protected); host: memory.max for Firecracker",
		"function killed inside the guest; VM, agent and neighbour fine; host limit not even needed",
		fmt.Sprintf("call failed: %q; same VM answered the next call (%s); firecracker memory.peak %s MiB of memory.max %s MiB, host OOM kills %d; neighbour median %s before, %s after",
			errText, res.StartKind, mib(peak), mib(max), hostKills, ms(before), ms(during)))
}

func mib(s string) string {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return s
	}
	return strconv.FormatInt(n>>20, 10)
}

func testCPUHog(t *testing.T, env *isoEnv) {
	deployAs(t, env.bob, "echo", "testfn", 10000)
	deployAs(t, env.alice, "cpuhog", "cpuhog", 30000)
	if _, _, err := invokeJSON(t, env.alice, "cpuhog", `{"ms":50}`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := invokeJSON(t, env.bob, "echo", `{"mode":"echo"}`); err != nil {
		t.Fatal(err)
	}
	vm := vmOf(t, env.alice, "cpuhog")
	before := neighbourLatency(t, env.bob, 10)
	u0 := cgroupStat(t, env.aliceID, vm.ID, "cpu.stat", "usage_usec")
	th0 := cgroupStat(t, env.aliceID, vm.ID, "cpu.stat", "nr_throttled")
	var during time.Duration
	done := make(chan struct{})
	t0 := time.Now()
	go func() {
		defer close(done)
		if _, _, err := invokeJSON(t, env.alice, "cpuhog", `{"ms":4000}`); err != nil {
			t.Errorf("cpu hog: %v", err)
		}
	}()
	time.Sleep(300 * time.Millisecond)
	during = neighbourLatency(t, env.bob, 15)
	<-done
	wall := time.Since(t0)
	used := time.Duration(cgroupStat(t, env.aliceID, vm.ID, "cpu.stat", "usage_usec")-u0) * time.Microsecond
	throttled := cgroupStat(t, env.aliceID, vm.ID, "cpu.stat", "nr_throttled") - th0
	share := float64(used) / float64(wall)
	if share < 0.35 || share > 0.65 {
		t.Errorf("VM used %.2f of a CPU, want about 0.5 (cpu.max 50%%)", share)
	}
	if throttled == 0 {
		t.Error("the VM was never throttled")
	}
	ok := neighbourOK(t, before, during)
	report(t, "CPU hog (busy loop for 4 s on its vCPU)",
		"cgroup cpu.max (server run with --cpu-limit 50: half a host CPU per vCPU)",
		"VM held to ~0.5 CPU; neighbour unaffected",
		fmt.Sprintf("VM used %.2f CPU over %s (%s CPU time), throttled %d times; neighbour median %s before, %s during (ok=%v)",
			share, wall.Round(time.Millisecond), used.Round(time.Millisecond), throttled, ms(before), ms(during), ok))
}

func testHostMemoryLimit(t *testing.T, env *isoEnv) {
	deployAs(t, env.bob, "echo", "testfn", 10000)
	deployAs(t, env.alice, "memhog", "memhog", 30000)
	if _, _, err := invokeJSON(t, env.alice, "memhog", `{"mib":16}`); err != nil {
		t.Fatal(err)
	}
	vm := vmOf(t, env.alice, "memhog")
	max := cgroupFile(t, env.aliceID, vm.ID, "memory.max")
	_, _, err := invokeJSON(t, env.alice, "memhog", `{"mib":56}`)
	if err == nil {
		t.Error("the call should fail: Firecracker is over its memory.max")
	}
	// The VM is gone (firecracker was killed): the pool notices and recycles it.
	gone := false
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline) && !gone; time.Sleep(50 * time.Millisecond) {
		vms, _ := env.alice.VMs(context.Background())
		gone = true
		for _, v := range vms {
			if v.ID == vm.ID {
				gone = false
			}
		}
	}
	if !gone {
		t.Errorf("VM %s still listed 10 s after its process was killed", vm.ID)
	}
	var ae *apiclient.APIError
	if !errors.As(err, &ae) || ae.Status != 500 {
		t.Errorf("want 500 (the VM died, not the function), got %v", err)
	}
	if _, _, err := invokeJSON(t, env.bob, "echo", `{"mode":"echo"}`); err != nil {
		t.Errorf("neighbour failed: %v", err)
	}
	errText := fmt.Sprint(err)
	if len(errText) > 90 {
		errText = errText[:90] + "..."
	}
	report(t, "Host memory limit (memory.max set below the guest's RAM, guest fills its memory)",
		"cgroup memory.max on the Firecracker process",
		"host kills that VM's Firecracker; neighbour fine",
		fmt.Sprintf("memory.max %s MiB for a 128 MiB guest; call failed (%s); VM removed; neighbour call ok", mib(max), errText))
}
