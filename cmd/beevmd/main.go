// Command beevmd is the BeeVM daemon. For now it has one subcommand, boot, which starts a
// single MicroVM, shows its console, stops it and cleans up. Later milestones add serve.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/om8P/BeeVM/internal/vmm"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "boot":
		os.Exit(runBoot(os.Args[2:]))
	case "invoke":
		os.Exit(runInvoke(os.Args[2:]))
	case "serve":
		os.Exit(runServe(os.Args[2:]))
	case "tenant":
		os.Exit(runTenant(os.Args[2:]))
	case "key":
		os.Exit(runKey(os.Args[2:]))
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: beevmd <command> [flags]")
	fmt.Fprintln(os.Stderr, "commands:")
	fmt.Fprintln(os.Stderr, "  boot     start one MicroVM, show its console, stop it and clean up")
	fmt.Fprintln(os.Stderr, "  invoke   boot a VM, upload a function binary, run it (repeatedly), clean up")
	fmt.Fprintln(os.Stderr, "  serve    run the HTTP API (deploy and invoke functions)")
	fmt.Fprintln(os.Stderr, "  tenant   add | ls   (manage customers; prints a new API key on add)")
	fmt.Fprintln(os.Stderr, "  key      add | ls | revoke   (manage API keys)")
}

// vmConfig builds the VM config shared by the subcommands.
func vmConfig(home, bootArgs string, vcpus, mem int64) vmm.Config {
	return vmm.Config{
		BaseDir:        filepath.Join(home, "run", "vms"),
		FirecrackerBin: filepath.Join(home, "bin", "firecracker"),
		KernelPath:     filepath.Join(home, "images", "vmlinux"),
		RootfsPath:     filepath.Join(home, "images", "rootfs.ext4"),
		ExtraBootArgs:  bootArgs,
		VCPUs:          vcpus,
		MemMiB:         mem,
	}
}

// beevmHome is where the binaries and images live (see scripts/common.sh).
func beevmHome() string {
	if h := os.Getenv("BEEVM_HOME"); h != "" {
		return h
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return ".beevm"
	}
	return filepath.Join(h, ".beevm")
}

func runBoot(args []string) int {
	fs := flag.NewFlagSet("boot", flag.ExitOnError)
	hold := fs.Duration("hold", 0, "keep the VM running this long after it is ready")
	timeout := fs.Duration("timeout", 10*time.Second, "give up if the VM is not ready in this time")
	keepLogs := fs.Bool("keep-logs", false, "keep console.log after cleanup")
	mem := fs.Int64("mem", 128, "guest memory in MiB")
	vcpus := fs.Int64("vcpus", 1, "number of virtual CPUs")
	_ = fs.Parse(args)

	home := beevmHome()
	vm, err := vmm.New(vmConfig(home, "beevm.mode=idle", *vcpus, *mem))
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	// Whatever happens below (error, Ctrl-C, panic), leave no process or folder behind.
	defer func() {
		if err := vm.Cleanup(*keepLogs); err != nil {
			fmt.Fprintln(os.Stderr, "cleanup error:", err)
		}
	}()

	// Ctrl-C cancels sigCtx. We then kill the VM instead of waiting for it.
	sigCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	fmt.Printf("== starting VM %s (%d vCPU, %d MiB)\n", vm.ID(), *vcpus, *mem)
	tailDone := make(chan struct{})
	var tailWG sync.WaitGroup
	tailWG.Add(1)
	go func() { defer tailWG.Done(); tailFile(vm.ConsolePath(), os.Stdout, tailDone) }()
	stopTail := func() { close(tailDone); tailWG.Wait() }

	startCtx, cancelStart := context.WithTimeout(sigCtx, *timeout)
	defer cancelStart()
	if err := vm.Start(startCtx); err != nil {
		stopTail()
		fmt.Fprintln(os.Stderr, "error:", err)
		return exitFor(sigCtx, 1)
	}
	took, err := vm.WaitForConsole(startCtx, "BEEVM_READY")
	if err != nil {
		stopTail()
		fmt.Fprintln(os.Stderr, "error:", err)
		return exitFor(sigCtx, 1)
	}
	fmt.Printf("\n== VM ready in %v (firecracker pid %d)\n", took.Round(time.Millisecond), vm.PID())

	if *hold > 0 {
		fmt.Printf("== holding for %v (Ctrl-C to kill)\n", *hold)
		select {
		case <-time.After(*hold):
		case <-sigCtx.Done():
			vm.Kill()
			stopTail()
			fmt.Println("\n== interrupted: VM killed, cleaning up")
			return 130
		}
	}

	stopStart := time.Now()
	stopCtx, cancelStop := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelStop()
	graceful, stopErr := vm.Stop(stopCtx)
	stopTail()
	how := "stopped gracefully"
	if !graceful {
		how = "had to be killed"
	}
	fmt.Printf("\n== summary: ready in %v, %s in %v\n", took.Round(time.Millisecond), how, time.Since(stopStart).Round(time.Millisecond))
	if stopErr != nil {
		fmt.Fprintln(os.Stderr, "stop error:", stopErr)
		return 1
	}
	return 0
}

// exitFor returns 130 (the shell convention for "interrupted") if Ctrl-C caused the stop.
func exitFor(sigCtx context.Context, code int) int {
	if sigCtx.Err() != nil {
		return 130
	}
	return code
}

// tailFile copies a growing file to w, like `tail -f`, until done is closed. It then copies
// whatever is left so the last lines are not lost.
func tailFile(path string, w io.Writer, done <-chan struct{}) {
	var f *os.File
	defer func() {
		if f != nil {
			f.Close()
		}
	}()
	buf := make([]byte, 4096)
	drain := func() {
		for f != nil {
			n, _ := f.Read(buf)
			if n == 0 {
				return
			}
			_, _ = w.Write(buf[:n])
		}
	}
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		if f == nil {
			f, _ = os.Open(path) // not there yet until Start creates it
		}
		drain()
		select {
		case <-done:
			if f == nil {
				f, _ = os.Open(path)
			}
			drain()
			return
		case <-tick.C:
		}
	}
}
