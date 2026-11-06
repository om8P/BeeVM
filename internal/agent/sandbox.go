package agent

// Inside the guest, the agent runs as root (it is started by init). User functions must not:
// a root function could kill the agent, fill /run, or change the VM's network settings. The
// VM boundary still protects the host and other tenants either way; these limits make sure a
// misbehaving function only breaks its own call, and the agent survives to report it.
//
//   - functions (and python/node shims) run as FunctionUID, not root,
//   - at most MaxFunctionProcs processes for that user (a fork bomb hits the wall),
//   - when guest memory runs out, the kernel's OOM killer picks the function, never the agent,
//   - after each call, processes the function left behind (even ones that escaped its process
//     group with setsid) are killed.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	// FunctionUID/GID is the guest user functions run as ("fn" in the image's /etc/passwd).
	FunctionUID = 1000
	FunctionGID = 1000
	// MaxFunctionProcs limits processes (and threads) of FunctionUID (RLIMIT_NPROC).
	MaxFunctionProcs = 256
)

// sandbox applies the limits. A nil *sandbox does nothing (the agent is not root, e.g. in unit
// tests on the host).
type sandbox struct {
	uid, gid int
}

func newSandbox() *sandbox {
	if os.Geteuid() != 0 {
		return nil
	}
	return &sandbox{uid: FunctionUID, gid: FunctionGID}
}

// HardenAgent protects the agent process itself. Call it once at start-up (as root).
func HardenAgent() error {
	if os.Geteuid() != 0 {
		return nil
	}
	// -1000: the OOM killer never picks the agent.
	if err := os.WriteFile("/proc/self/oom_score_adj", []byte("-1000"), 0); err != nil {
		return fmt.Errorf("oom_score_adj: %w", err)
	}
	// Inherited by every child. Root ignores it; FunctionUID does not.
	lim := &unix.Rlimit{Cur: MaxFunctionProcs, Max: MaxFunctionProcs}
	if err := unix.Setrlimit(unix.RLIMIT_NPROC, lim); err != nil {
		return fmt.Errorf("RLIMIT_NPROC: %w", err)
	}
	return nil
}

// WarmUp runs the sandbox's code paths once (start a process as the function user, then
// sweep). The agent calls it at start-up, before any snapshot is taken: a restored VM then
// finds this code and the kernel's matching paths already in its memory, instead of faulting
// them in during the first call (measured: the first call took ~24 ms longer without it).
func WarmUp() {
	sb := newSandbox()
	if sb == nil {
		return
	}
	cmd := exec.Command("/bin/true")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	sb.apply(cmd)
	if cmd.Start() == nil {
		sb.started(cmd.Process.Pid)
		_ = cmd.Wait()
	}
	sb.sweep(0)
}

// apply makes cmd run as the function user.
func (sb *sandbox) apply(cmd *exec.Cmd) {
	if sb == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Credential = &syscall.Credential{Uid: uint32(sb.uid), Gid: uint32(sb.gid), NoSetGroups: true}
}

// started runs right after cmd started: the child inherited the agent's -1000, so give it
// the opposite (the OOM killer's first choice).
func (sb *sandbox) started(pid int) {
	if sb == nil {
		return
	}
	_ = os.WriteFile(fmt.Sprintf("/proc/%d/oom_score_adj", pid), []byte("1000"), 0)
}

// sweep kills every process of the function user, except those in process group keep (the
// running shim; 0 = keep none). It returns how many it killed.
func (sb *sandbox) sweep(keep int) int {
	if sb == nil {
		return 0
	}
	// A fork bomb keeps forking while we kill it: killing frees room under the process limit,
	// and a survivor fills it at once. So first freeze: SIGSTOP every process we find, pass
	// after pass, until a pass finds none we have not stopped yet (a stopped process cannot
	// fork, so new ones can only come from processes not stopped yet). Then kill them all.
	stopped := map[int]bool{}
	for round := 0; round < 100; round++ {
		found := 0
		for _, pid := range userProcesses(sb.uid) {
			if stopped[pid] || (keep > 0 && processGroup(pid) == keep) {
				continue
			}
			_ = syscall.Kill(pid, syscall.SIGSTOP)
			stopped[pid] = true
			found++
		}
		if found == 0 {
			break
		}
	}
	for pid := range stopped {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	// The dead are collected by init (PID 1) a moment later; until then they are zombies,
	// which userProcesses skips.
	deadline := time.Now().Add(time.Second)
	for len(stopped) > 0 && time.Now().Before(deadline) {
		left := 0
		for _, pid := range userProcesses(sb.uid) {
			if stopped[pid] {
				left++
			}
		}
		if left == 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	return len(stopped)
}

// userProcesses lists the live (not zombie) processes whose UID is uid.
//
// It runs after every call, so it must be cheap: the owner of a /proc/<pid> folder is the
// process's user, so one stat per process filters out root's processes and the kernel's
// threads (most of the list) without reading and formatting their status files.
func userProcesses(uid int) []int {
	entries, _ := os.ReadDir("/proc")
	var out []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		var st syscall.Stat_t
		if syscall.Stat(filepath.Join("/proc", e.Name()), &st) != nil || int(st.Uid) != uid {
			continue
		}
		b, err := os.ReadFile(filepath.Join("/proc", e.Name(), "status"))
		if err != nil {
			continue
		}
		zombie := false
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "State:") && strings.Contains(line, "Z") {
				zombie = true // dead, only waiting to be collected; signals do nothing
			}
			if f := strings.Fields(strings.TrimPrefix(line, "Uid:")); strings.HasPrefix(line, "Uid:") && len(f) > 0 {
				if f[0] == strconv.Itoa(uid) && !zombie {
					out = append(out, pid)
				}
				break
			}
		}
	}
	return out
}

// processGroup reads a process's group id from /proc/<pid>/stat (field 5).
func processGroup(pid int) int {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return -1
	}
	// The command name (field 2) is in parentheses and may contain spaces: start after it.
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return -1
	}
	f := strings.Fields(s[i+1:]) // state, ppid, pgrp, ...
	if len(f) < 3 {
		return -1
	}
	g, _ := strconv.Atoi(f[2])
	return g
}
