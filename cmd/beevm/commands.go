package main

import (
	"bytes"
	"debug/elf"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/om8P/BeeVM/internal/agentproto"
	"github.com/om8P/BeeVM/internal/apiclient"
	"github.com/om8P/BeeVM/internal/apitypes"
	"github.com/om8P/BeeVM/internal/pkg"
)

// checkBinary makes sure path is a Linux x86-64 executable. A dynamically linked one is allowed
// but returns a warning: it only runs in the guest if the libraries it needs exist there.
func checkBinary(path string) (warning string, err error) {
	f, err := elf.Open(path)
	if err != nil {
		var fe *elf.FormatError
		if errors.As(err, &fe) {
			return "", fmt.Errorf("%s is not a Linux executable (ELF). Build a static Linux x86-64 binary, e.g. CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build", path)
		}
		return "", err
	}
	defer f.Close()
	if f.Machine != elf.EM_X86_64 || f.Class != elf.ELFCLASS64 {
		return "", fmt.Errorf("%s is built for %s, but the guest runs x86-64", path, f.Machine)
	}
	if f.Type != elf.ET_EXEC && f.Type != elf.ET_DYN {
		return "", fmt.Errorf("%s is not an executable", path)
	}
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return "this binary is dynamically linked. The guest is Alpine (musl), so a glibc binary will not start. Build it static (for Go: CGO_ENABLED=0).", nil
		}
	}
	return "", nil
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// ---------------------------------------------------------------- deploy

// packageCode returns the zip package to upload for a Python/Node.js function: a .zip file
// as it is, a folder or a single source file zipped in memory. For a single file without
// --handler, the handler defaults to "<file name>.handler".
func packageCode(path, handler string) (io.Reader, string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, "", err
	}
	if !info.IsDir() && strings.EqualFold(filepath.Ext(path), ".zip") {
		f, err := os.Open(path)
		return f, handler, err
	}
	if !info.IsDir() && handler == "" {
		handler = strings.TrimSuffix(info.Name(), filepath.Ext(info.Name())) + ".handler"
	}
	var buf bytes.Buffer
	if err := pkg.Zip(path, &buf); err != nil {
		return nil, "", err
	}
	return &buf, handler, nil
}

func newDeploy(s *settings) *cobra.Command {
	var memory, vcpus int64
	var timeout time.Duration
	var runtime, handler string
	cmd := &cobra.Command{
		Use:   "deploy <name> <binary|folder|file.py|file.js|package.zip>",
		Short: "Upload a function (create it, or replace the existing one)",
		Long: "Uploads a function. The runtime is detected from the path unless --runtime is given:\n" +
			"  binary  a static Linux x86-64 executable: the call's JSON payload arrives on stdin, the\n" +
			"          result JSON goes to stdout, logs go to stderr.\n" +
			"  python  a folder (or .zip) with handler.py, or one .py file: handler(event) is called with the\n" +
			"          payload and returns the result; print() output is the log.\n" +
			"  node    a folder (or .zip) with index.js, or one .js file: an exported (async) handler(event).\n" +
			"--handler picks another function, e.g. app.main (python: module.function, node: file.function).",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, path := args[0], args[1]
			if runtime == "" {
				detected, err := pkg.Detect(path)
				if err != nil {
					return err
				}
				runtime = detected
			}
			if !pkg.ValidRuntime(runtime) {
				return fmt.Errorf("--runtime must be binary, python or node")
			}
			var code io.Reader
			if runtime == agentproto.RuntimeBinary {
				if handler != "" {
					return fmt.Errorf("binaries have no handler")
				}
				warning, err := checkBinary(path)
				if err != nil {
					return err
				}
				if warning != "" {
					fmt.Fprintln(os.Stderr, "warning:", warning)
				}
				f, err := os.Open(path)
				if err != nil {
					return err
				}
				defer f.Close()
				code = f
			} else {
				var err error
				if code, handler, err = packageCode(path, handler); err != nil {
					return err
				}
				if c, ok := code.(io.Closer); ok {
					defer c.Close()
				}
			}
			c, err := s.client()
			if err != nil {
				return err
			}
			fn, created, err := c.Deploy(cmd.Context(), name, code, apitypes.FunctionConfig{
				Runtime: runtime, Handler: handler,
				MemoryMiB: memory, VCPUs: vcpus, TimeoutMS: timeout.Milliseconds(),
			})
			if err != nil {
				return err
			}
			verb := "updated"
			if created {
				verb = "created"
			}
			kind := fn.Runtime
			if fn.Handler != "" {
				kind += " " + fn.Handler
			}
			fmt.Printf("%s %s (%s): %s, %d MiB, %d vCPU, timeout %s\n", verb, fn.Name, kind, humanSize(fn.CodeSize),
				fn.MemoryMiB, fn.VCPUs, time.Duration(fn.TimeoutMS)*time.Millisecond)
			return nil
		},
	}
	cmd.Flags().StringVar(&runtime, "runtime", "", "binary, python or node (default: detected from the path)")
	cmd.Flags().StringVar(&handler, "handler", "", "python: module.function (default handler.handler); node: file.function (default index.handler)")
	cmd.Flags().Int64Var(&memory, "memory", 0, "memory in MiB (default 128, 64-1024)")
	cmd.Flags().Int64Var(&vcpus, "vcpus", 0, "virtual CPUs (default 1, 1-4)")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "time limit per call (default 10s, max 60s)")
	return cmd
}

// ---------------------------------------------------------------- invoke

func newInvoke(s *settings) *cobra.Command {
	var payload, payloadFile string
	var verbose bool
	cmd := &cobra.Command{
		Use:   "invoke <name>",
		Short: "Run a function and print its JSON output",
		Long: "The payload comes from --payload, or --payload-file, or standard input when something is piped in;\n" +
			"otherwise the function gets no input (null). With -v, timings are printed to stderr.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			body, err := readPayload(payload, payloadFile)
			if err != nil {
				return err
			}
			c, err := s.client()
			if err != nil {
				return err
			}
			start := time.Now()
			res, err := c.Invoke(cmd.Context(), args[0], body)
			rtt := time.Since(start)
			if err != nil {
				var ae *apiclient.APIError
				if errors.As(err, &ae) && ae.Status == 502 { // the function itself failed
					if ae.Stderr != "" {
						fmt.Fprintf(os.Stderr, "function log:\n%s\n", strings.TrimRight(ae.Stderr, "\n"))
					}
					return &exitError{1, "function failed: " + ae.Message}
				}
				return err
			}
			fmt.Println(strings.TrimSpace(string(res.Output)))
			if verbose {
				start := res.StartKind
				if start == "" {
					start = "boot" // older servers do not send it
				}
				if start == "warm" { // reused VM: nothing was started or uploaded
					fmt.Fprintf(os.Stderr, "vm %s: warm, function %d ms, server total %d ms, round trip %d ms\n",
						res.VMID, res.FunctionMS, res.TotalMS, rtt.Milliseconds())
				} else {
					if res.Snapshot != "" { // which snapshot: "function" = code was already inside
						start += " (" + res.Snapshot + " snapshot)"
					}
					fmt.Fprintf(os.Stderr, "vm %s: %s %d ms, upload %d ms, load %d ms, function %d ms, server total %d ms, round trip %d ms\n",
						res.VMID, start, res.BootMS, res.UploadMS, res.LoadMS, res.FunctionMS, res.TotalMS, rtt.Milliseconds())
				}
				if res.CodeCache != "" {
					fmt.Fprintf(os.Stderr, "code cache: %s, fetch %d ms\n", res.CodeCache, res.FetchMS)
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&payload, "payload", "", "JSON payload")
	cmd.Flags().StringVar(&payloadFile, "payload-file", "", "read the JSON payload from this file")
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "print timings to stderr")
	return cmd
}

// readPayload picks the payload source: flag, file, piped stdin, or nothing.
func readPayload(payload, file string) ([]byte, error) {
	switch {
	case payload != "" && file != "":
		return nil, errors.New("use only one of --payload and --payload-file")
	case payload != "":
		return []byte(payload), nil
	case file != "":
		return os.ReadFile(file)
	}
	if st, err := os.Stdin.Stat(); err == nil && st.Mode()&os.ModeCharDevice == 0 {
		return io.ReadAll(os.Stdin) // something is piped in
	}
	return nil, nil
}

// ---------------------------------------------------------------- ls and rm

func newLs(s *settings) *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List your functions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := s.client()
			if err != nil {
				return err
			}
			fns, err := c.List(cmd.Context())
			if err != nil {
				return err
			}
			if len(fns) == 0 {
				fmt.Println("no functions yet; deploy one with: beevm deploy <name> <binary|folder>")
				return nil
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tRUNTIME\tSIZE\tMEMORY\tVCPUS\tTIMEOUT\tUPDATED")
			for _, f := range fns {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%d MiB\t%d\t%s\t%s\n", f.Name, f.Runtime, humanSize(f.CodeSize), f.MemoryMiB, f.VCPUs,
					time.Duration(f.TimeoutMS)*time.Millisecond, f.UpdatedAt.Local().Format(time.DateTime))
			}
			return tw.Flush()
		},
	}
}

// ---------------------------------------------------------------- warm pool

func newPrewarm(s *settings) *cobra.Command {
	var n int
	cmd := &cobra.Command{
		Use:   "prewarm <name>",
		Short: "Start idle VMs for a function so its next calls are warm",
		Long: "Starts up to N VMs with the function's code loaded. They wait for calls and are removed\n" +
			"after the server's keep-alive time like any idle VM.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := s.client()
			if err != nil {
				return err
			}
			start := time.Now()
			res, err := c.Prewarm(cmd.Context(), args[0], n)
			if err != nil {
				return err
			}
			fmt.Printf("started %d of %d VMs for %s in %d ms\n", res.Started, res.Requested, args[0], time.Since(start).Milliseconds())
			return nil
		},
	}
	cmd.Flags().IntVarP(&n, "count", "n", 1, "how many VMs")
	return cmd
}

func newVMs(s *settings) *cobra.Command {
	return &cobra.Command{
		Use:   "vms",
		Short: "List your VMs in the warm pool",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := s.client()
			if err != nil {
				return err
			}
			vms, err := c.VMs(cmd.Context())
			if err != nil {
				return err
			}
			if len(vms) == 0 {
				fmt.Println("no VMs running")
				return nil
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "VM\tFUNCTION\tSTATE\tFOR\tCALLS\tADDRESS")
			for _, v := range vms {
				fn := v.Function
				if fn == "" {
					fn = "(removed)"
				}
				addr := v.Addr
				if addr == "" {
					addr = "-"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\n", v.ID, fn, v.State,
					time.Since(v.Since).Round(100*time.Millisecond), v.Calls, addr)
			}
			return tw.Flush()
		},
	}
}

func newRm(s *settings) *cobra.Command {
	return &cobra.Command{
		Use:   "rm <name>",
		Short: "Remove a function",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := s.client()
			if err != nil {
				return err
			}
			if err := c.Delete(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Println("removed", args[0])
			return nil
		},
	}
}
