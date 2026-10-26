package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/om8P/BeeVM/internal/snapshot"
	"github.com/om8P/BeeVM/internal/vmm"
)

// runInvoke starts a VM with the guest agent (a full boot, or with --restore a restore from the
// base snapshot), uploads a function binary, runs it one or more times, then stops the VM and
// cleans up.
func runInvoke(args []string) int {
	fs := flag.NewFlagSet("invoke", flag.ExitOnError)
	code := fs.String("code", "", "path to the function binary (static Linux x86-64 executable)")
	payload := fs.String("payload", "{}", "JSON payload sent to the function")
	repeat := fs.Int("repeat", 1, "how many times to invoke")
	fnTimeout := fs.Duration("timeout", 10*time.Second, "time limit for each function call")
	readyTimeout := fs.Duration("ready-timeout", 60*time.Second, "give up if the agent is not ready in this time")
	showConsole := fs.Bool("console", false, "stream the VM console while running (always shown on failure)")
	keepLogs := fs.Bool("keep-logs", false, "keep console.log after cleanup")
	mem := fs.Int64("mem", 128, "guest memory in MiB")
	vcpus := fs.Int64("vcpus", 1, "number of virtual CPUs")
	restore := fs.Bool("restore", false, "restore the VM from the base snapshot instead of booting (builds the snapshot if needed)")
	_ = fs.Parse(args)

	if *code == "" {
		fmt.Fprintln(os.Stderr, "error: --code is required (try: make examples, then --code bin/functions/hello-go-binary)")
		return 2
	}
	if !json.Valid([]byte(*payload)) {
		fmt.Fprintln(os.Stderr, "error: --payload is not valid JSON")
		return 2
	}
	if *repeat < 1 {
		fmt.Fprintln(os.Stderr, "error: --repeat must be at least 1")
		return 2
	}
	if _, err := os.Stat(*code); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}

	home := beevmHome()
	cfg := vmConfig(home, snapshot.BootArgs, *vcpus, *mem)
	cfg.SharedRootfs = true

	var vm *vmm.VM // set once the VM exists
	defer func() {
		if vm != nil {
			if err := vm.Cleanup(*keepLogs); err != nil {
				fmt.Fprintln(os.Stderr, "cleanup error:", err)
			}
		}
	}()

	sigCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	var tailWG sync.WaitGroup
	tailDone := make(chan struct{})
	tailing := false
	startTail := func() {
		if *showConsole && vm != nil {
			tailing = true
			tailWG.Add(1)
			go func() { defer tailWG.Done(); tailFile(vm.ConsolePath(), os.Stdout, tailDone) }()
		}
	}
	stopTail := func() {
		if tailing {
			close(tailDone)
			tailWG.Wait()
			tailing = false
		}
	}

	// fail prints why we stopped, plus the console (the guest's side of the story).
	fail := func(format string, a ...any) int {
		stopTail()
		if sigCtx.Err() != nil { // Ctrl-C: not an error, no console dump
			fmt.Println("\n== interrupted: VM killed, cleaning up")
			return 130
		}
		fmt.Fprintf(os.Stderr, "error: "+format+"\n", a...)
		if !*showConsole && vm != nil {
			if b, err := os.ReadFile(vm.ConsolePath()); err == nil && len(b) > 0 {
				fmt.Fprintf(os.Stderr, "--- VM console ---\n%s\n--- end of console ---\n", bytes.TrimRight(b, "\n"))
			}
		}
		return exitFor(sigCtx, 1)
	}

	// 1. Start the VM: a full boot, or a restore from the base snapshot.
	startCtx, cancelStart := context.WithTimeout(sigCtx, *readyTimeout)
	defer cancelStart()
	if *restore {
		snaps, err := snapshot.New(filepath.Join(home, "snapshots"), cfg, func(f string, a ...any) {
			fmt.Printf("== "+f+"\n", a...)
		})
		if err != nil {
			return fail("%v", err)
		}
		snap, err := snaps.Ensure(startCtx, *mem, *vcpus)
		if err != nil {
			return fail("snapshot: %v", err)
		}
		v, tm, err := vmm.Restore(startCtx, cfg, snap.Files)
		if err != nil {
			return fail("%v", err)
		}
		vm = v
		fmt.Printf("== restored VM %s from snapshot %s (%d vCPU, %d MiB): process %v, load %v\n",
			vm.ID(), snap.Key, *vcpus, *mem, tm.Spawn.Round(100*time.Microsecond), tm.Load.Round(100*time.Microsecond))
	} else {
		v, err := vmm.New(cfg)
		if err != nil {
			return fail("%v", err)
		}
		vm = v
		fmt.Printf("== starting VM %s (%d vCPU, %d MiB)\n", vm.ID(), *vcpus, *mem)
		startTail()
		if err := vm.Start(startCtx); err != nil {
			return fail("%v", err)
		}
	}
	if *restore {
		startTail()
	}
	ready, err := vm.WaitForAgent(startCtx)
	if err != nil {
		return fail("%v", err)
	}
	if *restore {
		// The restored guest's clock is behind and its random state is a copy: fix both.
		r, err := vm.Agent().Resume(startCtx)
		if err != nil {
			return fail("resume: %v", err)
		}
		ready = time.Since(vm.StartedAt())
		fmt.Printf("== agent ready after %v (guest clock moved %d ms forward)\n", ready.Round(100*time.Microsecond), r.ClockAdjustedMS)
	} else {
		fmt.Printf("== agent ready after %v\n", ready.Round(time.Millisecond))
	}

	// 2. Upload the function.
	upCtx, cancelUp := context.WithTimeout(sigCtx, 30*time.Second)
	defer cancelUp()
	t0 := time.Now()
	up, err := vm.Agent().UploadFunctionFile(upCtx, *code)
	if err != nil {
		return fail("upload: %v", err)
	}
	fmt.Printf("== uploaded %s (%d bytes, sha256 %s...) in %v\n", *code, up.Size, up.SHA256[:12], time.Since(t0).Round(time.Millisecond))

	// 3. Invoke.
	exit := 0
	var roundTrips []time.Duration
	for i := 1; i <= *repeat; i++ {
		callCtx, cancel := context.WithTimeout(sigCtx, *fnTimeout+10*time.Second)
		t0 := time.Now()
		out, err := vm.Agent().Invoke(callCtx, json.RawMessage(*payload), *fnTimeout)
		rt := time.Since(t0)
		cancel()
		if err != nil {
			return fail("invoke #%d: %v", i, err)
		}
		roundTrips = append(roundTrips, rt)
		if out.Error != "" {
			exit = 1
			fmt.Printf("#%d FAILED: %s (exit code %d, %dms in function, %v round trip)\n", i, out.Error, out.ExitCode, out.DurationMS, rt.Round(100*time.Microsecond))
			if out.Stdout != "" {
				fmt.Printf("   stdout: %s\n", strings.TrimSpace(out.Stdout))
			}
		} else {
			fmt.Printf("#%d %s  (%dms in function, %v round trip)\n", i, out.Output, out.DurationMS, rt.Round(100*time.Microsecond))
		}
		for _, line := range strings.Split(strings.TrimRight(out.Stderr, "\n"), "\n") {
			if line != "" {
				fmt.Printf("   log: %s\n", line)
			}
		}
	}

	// 4. Stop. A restored VM is disposable and is simply killed.
	stopStart := time.Now()
	stopCtx, cancelStop := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelStop()
	graceful, stopErr := vm.Stop(stopCtx)
	stopTail()
	how := "stopped gracefully"
	switch {
	case *restore:
		how = "killed (restored VMs are thrown away)"
	case !graceful:
		how = "had to be killed"
	}
	sort.Slice(roundTrips, func(a, b int) bool { return roundTrips[a] < roundTrips[b] })
	fmt.Printf("\n== summary: agent ready %v, %d call(s), median round trip %v, %s in %v\n",
		ready.Round(100*time.Microsecond), len(roundTrips), roundTrips[len(roundTrips)/2].Round(100*time.Microsecond),
		how, time.Since(stopStart).Round(time.Millisecond))
	if stopErr != nil {
		fmt.Fprintln(os.Stderr, "stop error:", stopErr)
		return 1
	}
	return exit
}
