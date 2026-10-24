package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/OmMhatre-99/BeeVM/internal/vmm"
)

// runInvoke boots a VM with the guest agent, uploads a function binary, runs it one or more
// times, then stops the VM and cleans up.
func runInvoke(args []string) int {
	fs := flag.NewFlagSet("invoke", flag.ExitOnError)
	code := fs.String("code", "", "path to the function binary (static Linux x86-64 executable)")
	payload := fs.String("payload", "{}", "JSON payload sent to the function")
	repeat := fs.Int("repeat", 1, "how many times to invoke")
	fnTimeout := fs.Duration("timeout", 10*time.Second, "time limit for each function call")
	readyTimeout := fs.Duration("ready-timeout", 20*time.Second, "give up if the agent is not ready in this time")
	showConsole := fs.Bool("console", false, "stream the VM console while running (always shown on failure)")
	keepLogs := fs.Bool("keep-logs", false, "keep console.log after cleanup")
	mem := fs.Int64("mem", 128, "guest memory in MiB")
	vcpus := fs.Int64("vcpus", 1, "number of virtual CPUs")
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

	vm, err := vmm.New(vmConfig(beevmHome(), "beevm.mode=agent", *vcpus, *mem))
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	defer func() {
		if err := vm.Cleanup(*keepLogs); err != nil {
			fmt.Fprintln(os.Stderr, "cleanup error:", err)
		}
	}()

	sigCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	// fail prints why we stopped, plus the console (the guest's side of the story).
	fail := func(format string, a ...any) int {
		if sigCtx.Err() != nil { // Ctrl-C: not an error, no console dump
			fmt.Println("\n== interrupted: VM killed, cleaning up")
			return 130
		}
		fmt.Fprintf(os.Stderr, "error: "+format+"\n", a...)
		if !*showConsole {
			if b, err := os.ReadFile(vm.ConsolePath()); err == nil && len(b) > 0 {
				fmt.Fprintf(os.Stderr, "--- VM console ---\n%s\n--- end of console ---\n", bytes.TrimRight(b, "\n"))
			}
		}
		return exitFor(sigCtx, 1)
	}

	var tailWG sync.WaitGroup
	tailDone := make(chan struct{})
	if *showConsole {
		tailWG.Add(1)
		go func() { defer tailWG.Done(); tailFile(vm.ConsolePath(), os.Stdout, tailDone) }()
	}
	stopTail := func() {
		if *showConsole {
			close(tailDone)
			tailWG.Wait()
		}
	}

	// 1. Boot and wait for the agent.
	startCtx, cancelStart := context.WithTimeout(sigCtx, *readyTimeout)
	defer cancelStart()
	fmt.Printf("== starting VM %s (%d vCPU, %d MiB)\n", vm.ID(), *vcpus, *mem)
	if err := vm.Start(startCtx); err != nil {
		stopTail()
		return fail("%v", err)
	}
	ready, err := vm.WaitForAgent(startCtx)
	if err != nil {
		stopTail()
		return fail("%v", err)
	}
	fmt.Printf("== agent ready after %v\n", ready.Round(time.Millisecond))

	// 2. Upload the function.
	upCtx, cancelUp := context.WithTimeout(sigCtx, 30*time.Second)
	defer cancelUp()
	t0 := time.Now()
	up, err := vm.Agent().UploadFunctionFile(upCtx, *code)
	if err != nil {
		stopTail()
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
			stopTail()
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

	// 4. Stop.
	stopStart := time.Now()
	stopCtx, cancelStop := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelStop()
	graceful, stopErr := vm.Stop(stopCtx)
	stopTail()
	how := "stopped gracefully"
	if !graceful {
		how = "had to be killed"
	}
	sort.Slice(roundTrips, func(a, b int) bool { return roundTrips[a] < roundTrips[b] })
	fmt.Printf("\n== summary: agent ready %v, %d call(s), median round trip %v, %s in %v\n",
		ready.Round(time.Millisecond), len(roundTrips), roundTrips[len(roundTrips)/2].Round(100*time.Microsecond),
		how, time.Since(stopStart).Round(time.Millisecond))
	if stopErr != nil {
		fmt.Fprintln(os.Stderr, "stop error:", stopErr)
		return 1
	}
	return exit
}
