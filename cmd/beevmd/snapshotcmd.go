package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/om8P/BeeVM/internal/snapshot"
)

// snapshotManager opens the Snapshot Manager for this BEEVM_HOME.
func snapshotManager(home string, logf func(string, ...any)) (*snapshot.Manager, error) {
	base := vmConfig(home, snapshot.BootArgs, 1, 128)
	base.BaseDir = filepath.Join(home, "run", "vms")
	m, err := snapshot.New(filepath.Join(home, "snapshots"), base, logf)
	if err != nil {
		return nil, err
	}
	for rt, path := range runtimeImages(home) {
		m.SetRootfs(rt, path)
	}
	return m, nil
}

// runSnapshot handles `beevmd snapshot build|ls|rm`.
func runSnapshot(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: beevmd snapshot build [--runtime binary|python|node] [--mem N] [--vcpus N] | ls | rm <key> | rm --stale")
		return 2
	}
	home := beevmHome()
	say := func(f string, a ...any) { fmt.Printf("== "+f+"\n", a...) }
	m, err := snapshotManager(home, say)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}

	switch args[0] {
	case "build":
		fs := flag.NewFlagSet("snapshot build", flag.ExitOnError)
		mem := fs.Int64("mem", 128, "guest memory in MiB")
		vcpus := fs.Int64("vcpus", 1, "number of virtual CPUs")
		rt := fs.String("runtime", "binary", "runtime snapshot to build: binary, python or node")
		_ = fs.Parse(args[1:])
		if !m.HasRuntime(*rt) {
			fmt.Fprintf(os.Stderr, "error: no image for runtime %q (run: make images)\n", *rt)
			return 1
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		s, err := m.EnsureRuntime(ctx, *rt, *mem, *vcpus)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		fmt.Printf("snapshot %s (%s runtime): %d MiB, %d vCPU, memory file %s, in %s\n",
			s.Key, s.RuntimeName(), s.Inputs.MemMiB, s.Inputs.VCPUs, humanBytes(s.MemBytes), s.Dir)
		return 0

	case "ls":
		list, err := m.List()
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		if len(list) == 0 {
			fmt.Println("no snapshots yet; build one with: beevmd snapshot build")
			return 0
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "KEY\tKIND\tRUNTIME\tFUNCTION\tMEMORY\tVCPUS\tSIZE\tBUILD\tCREATED\tSTATUS")
		for _, s := range list {
			status := "current"
			if !s.Current {
				status = "stale (images or build changed)"
			}
			fn := "-"
			if s.Function != nil {
				fn = fmt.Sprintf("%d (tenant %d)", s.Function.FunctionID, s.Function.TenantID)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d MiB\t%d\t%s\t%d ms\t%s\t%s\n", s.Key, s.Kind, s.RuntimeName(), fn,
				s.Inputs.MemMiB, s.Inputs.VCPUs, humanBytes(s.MemBytes), s.BuildMS,
				s.CreatedAt.Local().Format("2006-01-02 15:04:05"), status)
		}
		w.Flush()
		return 0

	case "rm":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: beevmd snapshot rm <key> | rm --stale")
			return 2
		}
		if args[1] == "--stale" {
			n, err := m.RemoveStale()
			if err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				return 1
			}
			fmt.Printf("removed %d stale snapshot(s)\n", n)
			return 0
		}
		if err := m.Remove(args[1]); err != nil {
			if errors.Is(err, snapshot.ErrNotFound) {
				fmt.Fprintf(os.Stderr, "error: no snapshot %q (see: beevmd snapshot ls)\n", args[1])
			} else {
				fmt.Fprintln(os.Stderr, "error:", err)
			}
			return 1
		}
		fmt.Printf("removed %s\n", args[1])
		return 0
	}
	fmt.Fprintf(os.Stderr, "unknown snapshot command %q\n", args[0])
	return 2
}

// humanBytes prints a size like 128 MiB.
func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%d MiB", n>>20)
	case n >= 1<<10:
		return fmt.Sprintf("%d KiB", n>>10)
	}
	return fmt.Sprintf("%d B", n)
}
