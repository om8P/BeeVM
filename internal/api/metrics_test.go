package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/om8P/BeeVM/internal/metrics"
	"github.com/om8P/BeeVM/internal/runner"
)

func newMetricsEnv(t *testing.T, logs io.Writer) (*env, *metrics.Metrics) {
	fr := &fakeRunner{result: runner.Result{
		Output: json.RawMessage(`{}`), VMID: "vm-7", StartKind: runner.StartRestore, Snapshot: runner.SnapshotFunction,
		BootMS: 45, FunctionMS: 3, TotalMS: 60,
	}}
	m := metrics.New()
	e := newEnvDeps(t, fr, fr, func(d *Deps) {
		d.Metrics = m
		if logs != nil {
			d.Logger = slog.New(slog.NewJSONHandler(logs, nil))
		}
	})
	return e, m
}

func scrape(t *testing.T, e *env) string {
	t.Helper()
	resp := e.do("GET", "/metrics", "", nil, "") // no key needed
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("/metrics status %d", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func TestMetricsCountInvocations(t *testing.T) {
	e, _ := newMetricsEnv(t, nil)
	key := e.tenant("alice")
	status(t, e.deploy(key, "hello", elf("v1"), ""), 201)
	status(t, e.do("POST", "/invoke/hello", key, nil, ""), 200)
	e.runner.result.StartKind, e.runner.result.Snapshot = runner.StartWarm, ""
	status(t, e.do("POST", "/invoke/hello", key, nil, ""), 200)
	e.runner.result.FunctionError = "exit status 1"
	status(t, e.do("POST", "/invoke/hello", key, nil, ""), 502)
	e.runner.result, e.runner.err = runner.Result{}, runner.ErrBusy
	status(t, e.do("POST", "/invoke/hello", key, nil, ""), 503)
	status(t, e.do("POST", "/invoke/nope", key, nil, ""), 404) // not a deployed function: not counted

	body := scrape(t, e)
	for _, want := range []string{
		`beevm_invocations_total{function="hello",runtime="binary",start="restore",status="ok",tenant="alice"} 1`,
		`beevm_invocations_total{function="hello",runtime="binary",start="warm",status="ok",tenant="alice"} 1`,
		`beevm_invocations_total{function="hello",runtime="binary",start="warm",status="function_error",tenant="alice"} 1`,
		`beevm_invocations_total{function="hello",runtime="binary",start="none",status="busy",tenant="alice"} 1`,
		`beevm_vm_ready_seconds_count{snapshot="function",start="restore"} 1`,
		`beevm_invoke_duration_seconds_count{runtime="binary",start="warm"} 2`,
		`beevm_code_fetches_total{cache="hit"} 4`,
		"go_goroutines",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics is missing %s", want)
		}
	}
	if strings.Contains(body, `function="nope"`) {
		t.Error("an unknown function name from the URL became a label")
	}
}

func TestNoMetricsRouteWithoutMetrics(t *testing.T) {
	e := newEnv(t)
	status(t, e.do("GET", "/metrics", "", nil, ""), 404)
}

func TestRequestLogHasInvokeDetails(t *testing.T) {
	var logs bytes.Buffer
	e, m := newMetricsEnv(t, &logs)
	key := e.tenant("alice")
	status(t, e.deploy(key, "hello", elf("v1"), ""), 201)
	logs.Reset()
	status(t, e.do("POST", "/invoke/hello", key, nil, ""), 200)

	var line map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &line); err != nil {
		t.Fatalf("log line is not one JSON object: %v\n%s", err, logs.String())
	}
	for k, want := range map[string]any{
		"function": "hello", "runtime": "binary", "start": "restore", "snapshot": "function",
		"boot_ms": 45.0, "function_ms": 3.0, "status": 200.0, "tenant": "alice",
	} {
		if line[k] != want {
			t.Errorf("log %s = %v, want %v", k, line[k], want)
		}
	}
	if id, _ := line["request_id"].(string); id == "" {
		t.Error("log has no request_id")
	}
	if n := testutil.CollectAndCount(m.Registry(), "beevm_invocations_total"); n != 1 {
		t.Errorf("%d invocation series, want 1", n)
	}
}
