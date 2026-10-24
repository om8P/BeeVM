//go:build kvm

package vmm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

// buildFunction compiles examples/functions/<name> as a static binary and returns its path.
func buildFunction(t *testing.T, name string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), name)
	cmd := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", out, "../../examples/functions/"+name)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", name, err, b)
	}
	return out
}

// bootAgent boots a VM whose init starts the guest agent, and waits until it answers.
func bootAgent(t *testing.T) *VM {
	t.Helper()
	cfg := kvmConfig(t)
	cfg.ExtraBootArgs = "beevm.mode=agent"
	vm, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = vm.Cleanup(false) })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := vm.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	took, err := vm.WaitForAgent(ctx)
	if err != nil {
		b, _ := os.ReadFile(vm.ConsolePath())
		t.Fatalf("WaitForAgent: %v\n--- console ---\n%s", err, b)
	}
	t.Logf("VM %s (pid %d): agent answered after %v", vm.ID(), vm.PID(), took)
	return vm
}

func TestAgentRunsFunctions(t *testing.T) {
	vm := bootAgent(t)
	pid := vm.PID()
	testfn := buildFunction(t, "testfn")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	agent := vm.Agent()

	up, err := agent.UploadFunctionFile(ctx, testfn)
	if err != nil || up.Size == 0 {
		t.Fatalf("upload: %+v, %v", up, err)
	}

	// 1. Twenty echo calls: every answer must match its own request.
	var times []time.Duration
	for i := 0; i < 20; i++ {
		start := time.Now()
		out, err := agent.Invoke(ctx, json.RawMessage(fmt.Sprintf(`{"mode":"echo","n":%d}`, i)), 5*time.Second)
		times = append(times, time.Since(start))
		if err != nil || out.Error != "" {
			t.Fatalf("call %d: %+v, %v", i, out, err)
		}
		var got struct {
			Echo struct {
				N int `json:"n"`
			} `json:"echo"`
		}
		if err := json.Unmarshal(out.Output, &got); err != nil || got.Echo.N != i {
			t.Fatalf("call %d: wrong output %s (%v)", i, out.Output, err)
		}
	}
	sort.Slice(times, func(a, b int) bool { return times[a] < times[b] })
	t.Logf("20 invokes over vsock: first-ish min %v, median %v, max %v", times[0], times[10], times[19])

	// 2. A failing function is reported, not hidden.
	out, err := agent.Invoke(ctx, json.RawMessage(`{"mode":"fail"}`), 5*time.Second)
	if err != nil || out.ExitCode != 3 || !strings.Contains(out.Stderr, "boom") || out.Error == "" {
		t.Errorf("fail: %+v, %v", out, err)
	}

	// 3. Bad output is reported.
	out, err = agent.Invoke(ctx, json.RawMessage(`{"mode":"garbage"}`), 5*time.Second)
	if err != nil || !strings.Contains(out.Error, "not valid JSON") {
		t.Errorf("garbage: %+v, %v", out, err)
	}

	// 4. A function that runs too long is killed, and the VM keeps working.
	start := time.Now()
	out, err = agent.Invoke(ctx, json.RawMessage(`{"mode":"sleep","ms":30000}`), 300*time.Millisecond)
	if err != nil || !strings.Contains(out.Error, "timeout") {
		t.Errorf("timeout: %+v, %v", out, err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("timeout call took %v", took)
	}
	out, err = agent.Invoke(ctx, json.RawMessage(`{"mode":"echo","n":99}`), 5*time.Second)
	if err != nil || out.Error != "" {
		t.Errorf("VM unusable after timeout: %+v, %v", out, err)
	}

	// 5. Clean shutdown, nothing left behind.
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer stopCancel()
	if graceful, err := vm.Stop(stopCtx); err != nil || !graceful {
		t.Errorf("Stop: graceful=%v err=%v", graceful, err)
	}
	if err := vm.Cleanup(false); err != nil {
		t.Fatal(err)
	}
	if processAlive(pid) {
		t.Errorf("firecracker pid %d still running", pid)
	}
	if _, err := os.Stat(vm.Dir()); !os.IsNotExist(err) {
		t.Error("vm folder was not removed")
	}
}

func TestAgentHelloExample(t *testing.T) {
	vm := bootAgent(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := vm.Agent().UploadFunctionFile(ctx, buildFunction(t, "hello-go-binary")); err != nil {
		t.Fatal(err)
	}
	out, err := vm.Agent().Invoke(ctx, json.RawMessage(`{"name":"Om"}`), 5*time.Second)
	if err != nil || out.Error != "" {
		t.Fatalf("invoke: %+v, %v", out, err)
	}
	var got map[string]string
	if err := json.Unmarshal(out.Output, &got); err != nil || got["greeting"] != "Hello, Om!" || got["runtime"] != "binary" {
		t.Errorf("output %s (%v)", out.Output, err)
	}
	if !strings.Contains(out.Stderr, "greeting Om") {
		t.Errorf("stderr should carry the function's log, got %q", out.Stderr)
	}
}

func TestAgentInvokeBeforeUploadFails(t *testing.T) {
	vm := bootAgent(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := vm.Agent().Invoke(ctx, json.RawMessage(`{}`), time.Second)
	if err == nil || !strings.Contains(err.Error(), "409") {
		t.Errorf("want a 409 error, got %v", err)
	}
}
