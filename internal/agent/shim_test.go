//go:build linux

package agent

import (
	"archive/zip"
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/om8P/BeeVM/internal/agentproto"
)

// newShimServer is newTestServer with the shims pointed at guest/shims/ in this repository,
// run by the interpreter on the test machine. Tests skip when the interpreter is missing.
func newShimServer(t *testing.T, runtime string) *httptest.Server {
	t.Helper()
	ts, s := newTestServer(t)
	shims, err := filepath.Abs("../../guest/shims")
	if err != nil {
		t.Fatal(err)
	}
	switch runtime {
	case agentproto.RuntimePython:
		if _, err := exec.LookPath("python3"); err != nil {
			t.Skip("python3 not installed")
		}
		s.shimCmds = map[string][]string{runtime: {"python3", "-I", "-B", "-u", filepath.Join(shims, "python", "shim.py")}}
	case agentproto.RuntimeNode:
		if _, err := exec.LookPath("node"); err != nil {
			t.Skip("node not installed (covered by the KVM tests)")
		}
		s.shimCmds = map[string][]string{runtime: {"node", filepath.Join(shims, "node", "shim.js")}}
	}
	t.Cleanup(func() {
		s.mu.Lock()
		s.stopShim()
		s.mu.Unlock()
	})
	return ts
}

func zipOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(body))
	}
	zw.Close()
	return buf.Bytes()
}

func putPackage(t *testing.T, ts *httptest.Server, data []byte) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPut, ts.URL+agentproto.PathPackage, bytes.NewReader(data))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func load(t *testing.T, ts *httptest.Server, runtime, handler string, timeoutMS int64) (int, string) {
	t.Helper()
	body, _ := json.Marshal(agentproto.LoadRequest{Runtime: runtime, Handler: handler, TimeoutMS: timeoutMS})
	resp, err := http.Post(ts.URL+agentproto.PathLoad, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

func mustLoad(t *testing.T, ts *httptest.Server, runtime, handler string) {
	t.Helper()
	if code, body := load(t, ts, runtime, handler, 0); code != 200 {
		t.Fatalf("load %s: %d %s", handler, code, body)
	}
}

func mustPackage(t *testing.T, ts *httptest.Server, files map[string]string) {
	t.Helper()
	if code, body := putPackage(t, ts, zipOf(t, files)); code != 200 {
		t.Fatalf("package: %d %s", code, body)
	}
}

const pyHello = `
import os
print("imported once", os.getpid())
calls = 0

def handler(event):
    global calls
    calls += 1
    name = (event or {}).get("name") or "world"
    print("hello", name)
    if name == "boom":
        raise ValueError("boom")
    if name == "sleep":
        import time; time.sleep(5)
    if name == "exit":
        os._exit(3)
    if name == "object":
        return object()
    if name == "nan":
        return float("nan")
    return {"greeting": "Hello, " + name + "!", "calls": calls, "pid": os.getpid()}
`

func TestPythonRuntimeSnapshotStepThenHandler(t *testing.T) {
	ts := newShimServer(t, "python")
	// Runtime snapshot step: interpreter only, before any package exists.
	mustLoad(t, ts, "python", "")
	if code, out := invoke(t, ts, `{}`, 0); code != http.StatusConflict {
		t.Fatalf("invoke without handler: %d %+v", code, out)
	}
	mustPackage(t, ts, map[string]string{"handler.py": pyHello})
	code, body := load(t, ts, "python", "handler.handler", 0)
	if code != 200 || !strings.Contains(body, "imported once") {
		t.Fatalf("load: %d %s", code, body)
	}
	var pid any
	for i := 1; i <= 3; i++ {
		code, out := invoke(t, ts, `{"name":"Om"}`, 0)
		if code != 200 || out.Error != "" {
			t.Fatalf("invoke %d: %d %+v", i, code, out)
		}
		var res struct {
			Greeting string
			Calls    int
			PID      any
		}
		json.Unmarshal(out.Output, &res)
		if res.Greeting != "Hello, Om!" || res.Calls != i {
			t.Fatalf("call %d: %s", i, out.Output)
		}
		if i == 1 {
			pid = res.PID
		} else if res.PID != pid {
			t.Fatal("the shim was restarted between calls; it should stay loaded")
		}
		if !strings.Contains(out.Stderr, "hello Om") || strings.Contains(out.Stderr, "imported once") {
			t.Fatalf("logs of one call: %q", out.Stderr)
		}
	}
}

func TestPythonFunctionErrors(t *testing.T) {
	ts := newShimServer(t, "python")
	mustPackage(t, ts, map[string]string{"handler.py": pyHello})
	mustLoad(t, ts, "python", "handler.handler")

	_, out := invoke(t, ts, `{"name":"boom"}`, 0)
	if out.Error != "function raised an error: ValueError: boom" || out.ExitCode != 1 ||
		!strings.Contains(out.Stderr, "Traceback") || !strings.Contains(out.Stderr, "hello boom") {
		t.Fatalf("raise: %+v", out)
	}
	for _, name := range []string{"object", "nan"} {
		_, out = invoke(t, ts, `{"name":"`+name+`"}`, 0)
		if !strings.HasPrefix(out.Error, "function raised an error:") {
			t.Fatalf("%s: %+v", name, out)
		}
	}
	// A failing call keeps the runtime: the next call works.
	if _, out = invoke(t, ts, `{"name":"x"}`, 0); out.Error != "" {
		t.Fatalf("after errors: %+v", out)
	}
}

func TestPythonTimeoutAndExitStopTheRuntime(t *testing.T) {
	ts := newShimServer(t, "python")
	mustPackage(t, ts, map[string]string{"handler.py": pyHello})
	mustLoad(t, ts, "python", "handler.handler")
	start := time.Now()
	_, out := invoke(t, ts, `{"name":"sleep"}`, 300)
	if !strings.HasPrefix(out.Error, "timeout after") || time.Since(start) > 4*time.Second {
		t.Fatalf("timeout: %+v after %v", out, time.Since(start))
	}
	if code, _ := invoke(t, ts, `{}`, 0); code != http.StatusConflict {
		t.Fatalf("after a timeout the runtime is gone, got %d", code)
	}

	mustLoad(t, ts, "python", "handler.handler")
	_, out = invoke(t, ts, `{"name":"exit"}`, 0)
	if !strings.HasPrefix(out.Error, "runtime exited") {
		t.Fatalf("exit: %+v", out)
	}
}

func TestPythonImportErrors(t *testing.T) {
	ts := newShimServer(t, "python")
	cases := map[string]struct {
		files         map[string]string
		handler, want string
	}{
		"syntax":      {map[string]string{"handler.py": "def handler(:\n"}, "handler.handler", "SyntaxError"},
		"raises":      {map[string]string{"handler.py": "raise RuntimeError('no db')\n"}, "handler.handler", "no db"},
		"no function": {map[string]string{"handler.py": "x = 1\n"}, "handler.handler", "no function 'handler'"},
		"no module":   {map[string]string{"handler.py": "x = 1\n"}, "other.handler", "No module named 'other'"},
		"exit":        {map[string]string{"handler.py": "import sys; sys.exit(2)\n"}, "handler.handler", "SystemExit"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			mustPackage(t, ts, c.files)
			code, body := load(t, ts, "python", c.handler, 0)
			if code != http.StatusUnprocessableEntity || !strings.Contains(body, c.want) {
				t.Fatalf("got %d %s, want 422 with %q", code, body, c.want)
			}
		})
	}
}

func TestPackageReplacesCodeAndRestartsRuntime(t *testing.T) {
	ts := newShimServer(t, "python")
	mustPackage(t, ts, map[string]string{"handler.py": "def handler(e): return 1"})
	mustLoad(t, ts, "python", "handler.handler")
	if _, out := invoke(t, ts, `{}`, 0); string(out.Output) != "1" {
		t.Fatalf("v1: %+v", out)
	}
	mustPackage(t, ts, map[string]string{"handler.py": "def handler(e): return 2", "lib/x.py": ""})
	if code, _ := invoke(t, ts, `{}`, 0); code != http.StatusConflict {
		t.Fatal("old code must not keep running after a new package")
	}
	mustLoad(t, ts, "python", "handler.handler")
	if _, out := invoke(t, ts, `{}`, 0); string(out.Output) != "2" {
		t.Fatalf("v2: %+v", out)
	}
}

func TestPackageAndLoadRejects(t *testing.T) {
	ts, s := newTestServer(t)
	if code, body := putPackage(t, ts, zipOf(t, map[string]string{"../escape.py": "x"})); code != 400 || !strings.Contains(body, "outside") {
		t.Fatalf("zip slip: %d %s", code, body)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(s.codeDir), "escape.py")); err == nil {
		t.Fatal("zip slip wrote a file")
	}
	if code, _ := putPackage(t, ts, []byte("not a zip")); code != 400 {
		t.Fatalf("not a zip: %d", code)
	}
	if code, _ := load(t, ts, "ruby", "", 0); code != 400 {
		t.Fatalf("unknown runtime: %d", code)
	}
	if code, _ := load(t, ts, "python", "../x.f", 0); code != 400 {
		t.Fatalf("bad handler: %d", code)
	}
	if code, _ := load(t, ts, "python", "handler.handler", 0); code != http.StatusConflict {
		t.Fatalf("handler before package: %d", code)
	}
}

func TestNodeRuntime(t *testing.T) {
	ts := newShimServer(t, "node")
	mustLoad(t, ts, "node", "")
	mustPackage(t, ts, map[string]string{
		"index.js": `console.log("imported once");
let calls = 0;
exports.handler = async (event) => {
  calls++;
  console.log("hello", event.name);
  if (event.name === "boom") throw new Error("boom");
  if (event.name === "sleep") await new Promise(r => setTimeout(r, 5000));
  return { greeting: "Hello, " + event.name + "!", calls };
};`,
		"esm.mjs": `export function handler(event, context) { return { ctx: context.function }; }`,
	})
	code, body := load(t, ts, "node", "index.handler", 0)
	if code != 200 || !strings.Contains(body, "imported once") {
		t.Fatalf("load: %d %s", code, body)
	}
	for i := 1; i <= 2; i++ {
		_, out := invoke(t, ts, `{"name":"Om"}`, 0)
		if out.Error != "" || !strings.Contains(string(out.Output), `"calls":`+string(rune('0'+i))) || !strings.Contains(out.Stderr, "hello Om") {
			t.Fatalf("call %d: %+v", i, out)
		}
	}
	_, out := invoke(t, ts, `{"name":"boom"}`, 0)
	if out.Error != "function raised an error: Error: boom" {
		t.Fatalf("throw: %+v", out)
	}
	_, out = invoke(t, ts, `{"name":"sleep"}`, 300)
	if !strings.HasPrefix(out.Error, "timeout after") {
		t.Fatalf("timeout: %+v", out)
	}
	mustLoad(t, ts, "node", "esm.handler")
	if _, out := invoke(t, ts, `{}`, 0); string(out.Output) != `{"ctx":"esm.handler"}` {
		t.Fatalf("esm with context: %+v", out)
	}
}

func TestReadLine(t *testing.T) {
	r := bufio.NewReaderSize(strings.NewReader(strings.Repeat("x", 100)+"\nok\n"), 16)
	if _, err := readLine(r, 50); !errors.Is(err, errAnswerBig) {
		t.Fatalf("want errAnswerBig, got %v", err)
	}
	// The long line was consumed: the next answer is intact.
	if line, err := readLine(r, 50); err != nil || string(line) != "ok\n" {
		t.Fatalf("next line %q %v", line, err)
	}
	if _, err := readLine(r, 50); err != io.EOF {
		t.Fatalf("want EOF, got %v", err)
	}
}

func TestSummary(t *testing.T) {
	py := "Traceback (most recent call last):\n  File \"x\"\nKeyError: 'a'\n"
	if got := summary("python", py); got != "KeyError: 'a'" {
		t.Fatal(got)
	}
	if got := summary("node", "TypeError: x is undefined\n    at handler (index.js:1)"); got != "TypeError: x is undefined" {
		t.Fatal(got)
	}
}
