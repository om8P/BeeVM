//go:build kvm

package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/om8P/BeeVM/internal/apiclient"
	"github.com/om8P/BeeVM/internal/apitypes"
	"github.com/om8P/BeeVM/internal/pkg"
	"github.com/om8P/BeeVM/internal/pool"
	"github.com/om8P/BeeVM/internal/runner"
	"github.com/om8P/BeeVM/internal/snapshot"
	"github.com/om8P/BeeVM/internal/vmm"
)

// End to end with Python and Node.js: go test -tags kvm -v -run E2ERuntime ./internal/api/

// runtimeServer starts the API with a WarmRunner that knows the python and node images, like
// `beevmd serve` does. A short keep-alive lets tests force a fresh VM by waiting.
func runtimeServer(t *testing.T, keepAlive time.Duration) (*apiclient.Client, *runner.WarmRunner, *snapshot.Manager, string) {
	t.Helper()
	var w *runner.WarmRunner
	var snaps *snapshot.Manager
	c, _, vmBase := e2eServerWith(t, func(base vmm.Config) runner.Runner {
		m, err := snapshot.New(t.TempDir(), base, t.Logf)
		if err != nil {
			t.Fatal(err)
		}
		w = runner.NewWarmRunner(base, m, pool.Config{MaxVMs: 4, KeepAlive: keepAlive, QueueWait: 60 * time.Second},
			slog.New(slog.NewTextHandler(io.Discard, nil)))
		for _, rt := range []string{"python", "node"} {
			img := filepath.Join(filepath.Dir(base.RootfsPath), "rootfs-"+rt+".ext4")
			if _, err := os.Stat(img); err != nil {
				t.Skipf("skipping, %v (run `make images`)", err)
			}
			m.SetRootfs(rt, img)
			w.SetRootfs(rt, img)
		}
		snaps = m
		return w
	})
	t.Cleanup(func() { _ = w.Close() })
	return c, w, snaps, vmBase
}

// zipOfDir packages a folder the way `beevm deploy` does.
func zipOfDir(t *testing.T, dir string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "pkg.zip")
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := pkg.Zip(dir, f); err != nil {
		t.Fatal(err)
	}
	return out
}

// sourceDir writes files into a new folder.
func sourceDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// functionSnapshots lists the function snapshots on disk.
func functionSnapshots(t *testing.T, m *snapshot.Manager) []snapshot.Info {
	t.Helper()
	list, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	var out []snapshot.Info
	for _, s := range list {
		if s.IsFunction() {
			out = append(out, s)
		}
	}
	return out
}

// waitFunctionSnapshots waits until there are n function snapshots (built in the background).
func waitFunctionSnapshots(t *testing.T, m *snapshot.Manager, n int) []snapshot.Info {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		if fs := functionSnapshots(t, m); len(fs) == n {
			return fs
		}
		if time.Now().After(deadline) {
			t.Fatalf("want %d function snapshots, have %d", n, len(functionSnapshots(t, m)))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestE2ERuntimeHelloPythonAndNode(t *testing.T) {
	c, w, snaps, vmBase := runtimeServer(t, 300*time.Millisecond)
	ctx := context.Background()
	for i, fn := range []string{"hello-python", "hello-node"} {
		rt := strings.TrimPrefix(fn, "hello-")
		t.Run(rt, func(t *testing.T) {
			deployFile(t, c, fn, zipOfDir(t, "../../examples/functions/"+fn), apitypes.FunctionConfig{Runtime: rt})

			// The deploy started a function snapshot build; once it is there, a new VM comes
			// from it: no upload, no import.
			fs := waitFunctionSnapshots(t, snaps, i+1)
			res, err := c.Invoke(ctx, fn, []byte(`{"name":"Om"}`))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(res.Output), `"greeting":"Hello, Om!"`) || !strings.Contains(string(res.Output), `"runtime":"`+rt+`"`) {
				t.Fatalf("output %s", res.Output)
			}
			if res.StartKind != "restore" || res.Snapshot != "function" || res.UploadMS != 0 || res.LoadMS != 0 {
				t.Fatalf("first call should restore the function snapshot: %+v", res)
			}
			t.Logf("%s from function snapshot: ready %d ms, function %d ms, server total %d ms (snapshot built in %d ms)",
				rt, res.BootMS, res.FunctionMS, res.TotalMS, fs[len(fs)-1].BuildMS)

			// The next call reuses the VM.
			if r, err := c.Invoke(ctx, fn, []byte(`{"name":"again"}`)); err != nil || r.StartKind != "warm" || r.VMID != res.VMID {
				t.Fatalf("second call: %+v %v", r, err)
			}

			// Without the function snapshot a VM comes from the runtime snapshot and imports.
			waitVMs(t, c, 0, 3*time.Second)
			var fnID int64
			for _, s := range functionSnapshots(t, snaps) {
				if s.RuntimeName() == rt {
					fnID = s.Function.FunctionID
				}
			}
			w.ForgetFunction(fnID)
			res2, err := c.Invoke(ctx, fn, []byte(`{"name":"Om"}`))
			if err != nil || res2.Snapshot != "runtime" || res2.UploadMS < 0 || res2.LoadMS == 0 {
				t.Fatalf("runtime snapshot call: %+v %v", res2, err)
			}
			t.Logf("%s from runtime snapshot: ready %d ms, upload %d ms, import %d ms, server total %d ms",
				rt, res2.BootMS, res2.UploadMS, res2.LoadMS, res2.TotalMS)
			// ...and that call started the function snapshot build again.
			waitFunctionSnapshots(t, snaps, i+1)
			waitVMs(t, c, 0, 3*time.Second)
		})
	}
	_ = w.Close()
	waitNoVMLeft(t, vmBase)
}

func TestE2ERuntimeFailures(t *testing.T) {
	c, w, _, vmBase := runtimeServer(t, time.Minute)
	ctx := context.Background()
	wantFail := func(name, payload, contains, inLog string) {
		t.Helper()
		_, err := c.Invoke(ctx, name, []byte(payload))
		var ae *apiclient.APIError
		if !errors.As(err, &ae) || ae.Status != 502 || !strings.Contains(ae.Message, contains) || !strings.Contains(ae.Stderr, inLog) {
			t.Fatalf("%s %s: want 502 with %q (log %q), got %v", name, payload, contains, inLog, err)
		}
	}

	// Code that does not import: every call fails with the traceback as the log.
	deployFile(t, c, "broken", zipOfDir(t, sourceDir(t, map[string]string{"handler.py": "def handler(event:\n"})),
		apitypes.FunctionConfig{Runtime: "python"})
	wantFail("broken", `{}`, "function failed to load: SyntaxError", "Traceback")

	py := sourceDir(t, map[string]string{"handler.py": `
import os, time
def handler(event):
    if event == "raise": raise ValueError("boom")
    if event == "sleep": time.sleep(5)
    if event == "exit": os._exit(3)
    return {"pid": os.getpid()}
`})
	deployFile(t, c, "py", zipOfDir(t, py), apitypes.FunctionConfig{Runtime: "python", TimeoutMS: 500})
	first, err := c.Invoke(ctx, "py", []byte(`"ok"`))
	if err != nil {
		t.Fatal(err)
	}
	// A handler that raises leaves a fine VM: the next call is warm on the same VM.
	wantFail("py", `"raise"`, "function raised an error: ValueError: boom", "Traceback")
	if r, err := c.Invoke(ctx, "py", []byte(`"ok"`)); err != nil || r.StartKind != "warm" || r.VMID != first.VMID {
		t.Fatalf("after a raise: %+v %v", r, err)
	}
	// A timeout or a dead runtime throws the VM away.
	wantFail("py", `"sleep"`, "timeout after", "")
	r, err := c.Invoke(ctx, "py", []byte(`"ok"`))
	if err != nil || r.StartKind == "warm" {
		t.Fatalf("after a timeout the VM must be new: %+v %v", r, err)
	}
	wantFail("py", `"exit"`, "runtime exited", "")
	if r2, err := c.Invoke(ctx, "py", []byte(`"ok"`)); err != nil || r2.VMID == r.VMID {
		t.Fatalf("after the runtime exited the VM must be new: %+v %v", r2, err)
	}

	// Node: a thrown error is a function failure; an async handler works.
	node := sourceDir(t, map[string]string{"index.js": `
exports.handler = async (e) => { if (e === "throw") throw new Error("boom"); await null; return { ok: e }; };`})
	deployFile(t, c, "js", zipOfDir(t, node), apitypes.FunctionConfig{Runtime: "node"})
	if r, err := c.Invoke(ctx, "js", []byte(`"x"`)); err != nil || string(r.Output) != `{"ok":"x"}` {
		t.Fatalf("node: %+v %v", r, err)
	}
	wantFail("js", `"throw"`, "function raised an error: Error: boom", "at ")

	for _, name := range []string{"broken", "py", "js"} {
		_ = c.Delete(ctx, name)
	}
	_ = w.Close()
	waitNoVMLeft(t, vmBase)
}

// Function snapshots belong to one function: two functions with the same code get their own,
// and deleting or redeploying a function removes its snapshot.
func TestE2ERuntimeFunctionSnapshotsArePerFunction(t *testing.T) {
	c, w, snaps, vmBase := runtimeServer(t, time.Minute)
	ctx := context.Background()
	pkgPath := zipOfDir(t, "../../examples/functions/hello-python")
	deployFile(t, c, "a", pkgPath, apitypes.FunctionConfig{Runtime: "python"})
	deployFile(t, c, "b", pkgPath, apitypes.FunctionConfig{Runtime: "python"})
	fs := waitFunctionSnapshots(t, snaps, 2)
	if fs[0].Function.FunctionID == fs[1].Function.FunctionID || fs[0].Function.CodeSHA256 != fs[1].Function.CodeSHA256 {
		t.Fatalf("snapshots: %+v / %+v", fs[0].Function, fs[1].Function)
	}

	if err := c.Delete(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if left := functionSnapshots(t, snaps); len(left) != 1 {
		t.Fatalf("after delete: %d function snapshots", len(left))
	}
	// Redeploy b with other code: its old snapshot goes, a new one is built.
	deployFile(t, c, "b", zipOfDir(t, sourceDir(t, map[string]string{"handler.py": "def handler(e): return 2"})),
		apitypes.FunctionConfig{Runtime: "python"})
	waitFunctionSnapshots(t, snaps, 1)
	if r, err := c.Invoke(ctx, "b", nil); err != nil || string(r.Output) != "2" || r.Snapshot != "function" {
		t.Fatalf("b after redeploy: %+v %v", r, err)
	}
	_ = c.Delete(ctx, "b")
	if left := functionSnapshots(t, snaps); len(left) != 0 {
		t.Fatalf("after deleting everything: %d function snapshots", len(left))
	}
	_ = w.Close()
	waitNoVMLeft(t, vmBase)
}
