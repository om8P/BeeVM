package api

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/om8P/BeeVM/internal/apitypes"
	"github.com/om8P/BeeVM/internal/pool"
	"github.com/om8P/BeeVM/internal/runner"
)

// fakePoolRunner is a fakeRunner with the warm pool's extras.
type fakePoolRunner struct {
	*fakeRunner
	mu         sync.Mutex
	drained    []int64
	prewarmReq runner.Request
	prewarmN   int
	prewarmErr error
	vms        map[int64][]pool.VMInfo // by tenant
}

func (f *fakePoolRunner) Prewarm(ctx context.Context, req runner.Request, n int) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prewarmReq, f.prewarmN = req, n
	if f.prewarmErr != nil {
		return 0, f.prewarmErr
	}
	return n, nil
}
func (f *fakePoolRunner) Drain(id int64) {
	f.mu.Lock()
	f.drained = append(f.drained, id)
	f.mu.Unlock()
}
func (f *fakePoolRunner) VMs(tenantID int64) []pool.VMInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.vms[tenantID]
}
func (f *fakePoolRunner) MaxVMs() int { return 4 }

func newPoolEnv(t *testing.T) (*env, *fakePoolRunner) {
	fr := &fakeRunner{result: runner.Result{Output: json.RawMessage(`{}`), VMID: "vm-1", StartKind: runner.StartWarm, FunctionMS: 3}}
	pr := &fakePoolRunner{fakeRunner: fr, vms: map[int64][]pool.VMInfo{}}
	return newEnvWith(t, pr, fr), pr
}

func TestInvokeWarmHeadersAndPoolKey(t *testing.T) {
	e, pr := newPoolEnv(t)
	key := e.tenant("alice")
	status(t, e.deploy(key, "hello", elf("v1"), ""), 201)
	resp := e.do("POST", "/invoke/hello", key, nil, "")
	if got := resp.Header.Get(apitypes.HeaderStart); got != "warm" {
		t.Errorf("start header %q", got)
	}
	status(t, resp, 200)
	f, _ := e.reg.GetFunction(pr.last.TenantID, "hello")
	if pr.last.FunctionID != f.ID || pr.last.CodeSHA256 != f.CodeSHA256 || pr.last.TenantID == 0 {
		t.Errorf("request lacks the function identity: %+v", pr.last)
	}
}

func TestRedeployAndDeleteDrain(t *testing.T) {
	e, pr := newPoolEnv(t)
	key := e.tenant("alice")
	status(t, e.deploy(key, "hello", elf("v1"), ""), 201)
	if len(pr.drained) != 0 {
		t.Fatal("a new function has nothing to drain")
	}
	status(t, e.deploy(key, "hello", elf("v2"), ""), 200)
	tn, _ := e.reg.GetTenant("alice")
	f, _ := e.reg.GetFunction(tn.ID, "hello")
	status(t, e.do("DELETE", "/api/v1/functions/hello", key, nil, ""), 204)
	if len(pr.drained) != 2 || pr.drained[0] != f.ID || pr.drained[1] != f.ID {
		t.Errorf("drained %v, want [%d %d]", pr.drained, f.ID, f.ID)
	}
}

func TestPrewarm(t *testing.T) {
	e, pr := newPoolEnv(t)
	key := e.tenant("alice")
	status(t, e.deploy(key, "hello", elf("v1"), `{"memory_mib":256}`), 201)

	res := decode[apitypes.PrewarmResult](t, e.do("POST", "/api/v1/functions/hello/prewarm?count=3", key, nil, ""))
	if res.Requested != 3 || res.Started != 3 || pr.prewarmN != 3 {
		t.Errorf("got %+v (runner asked for %d)", res, pr.prewarmN)
	}
	if pr.prewarmReq.MemoryMiB != 256 || pr.prewarmReq.FunctionID == 0 || pr.prewarmReq.CodePath == "" {
		t.Errorf("prewarm request %+v", pr.prewarmReq)
	}
	res = decode[apitypes.PrewarmResult](t, e.do("POST", "/api/v1/functions/hello/prewarm", key, nil, ""))
	if res.Requested != 1 {
		t.Errorf("default count: %+v", res)
	}
	for _, bad := range []string{"0", "5", "x"} { // MaxVMs is 4
		status(t, e.do("POST", "/api/v1/functions/hello/prewarm?count="+bad, key, nil, ""), 400)
	}
	status(t, e.do("POST", "/api/v1/functions/nope/prewarm", key, nil, ""), 404)
	// Another tenant cannot prewarm alice's function.
	status(t, e.do("POST", "/api/v1/functions/hello/prewarm", e.tenant("bob"), nil, ""), 404)
	status(t, e.do("POST", "/api/v1/functions/hello/prewarm", "", nil, ""), 401)

	pr.prewarmErr = runner.ErrBusy
	resp := e.do("POST", "/api/v1/functions/hello/prewarm", key, nil, "")
	if resp.Header.Get("Retry-After") == "" {
		t.Error("no Retry-After")
	}
	status(t, resp, 503)
}

func TestPrewarmWithoutPoolIs501(t *testing.T) {
	e := newEnv(t)
	key := e.tenant("alice")
	status(t, e.deploy(key, "hello", elf("v1"), ""), 201)
	status(t, e.do("POST", "/api/v1/functions/hello/prewarm", key, nil, ""), 501)
	l := decode[apitypes.VMList](t, e.do("GET", "/api/v1/vms", key, nil, ""))
	if l.VMs == nil || len(l.VMs) != 0 {
		t.Errorf("without a pool the list is empty: %+v", l)
	}
}

func TestListVMsIsPerTenant(t *testing.T) {
	e, pr := newPoolEnv(t)
	alice, bob := e.tenant("alice"), e.tenant("bob")
	status(t, e.deploy(alice, "hello", elf("v1"), ""), 201)
	ta, _ := e.reg.GetTenant("alice")
	tb, _ := e.reg.GetTenant("bob")
	f, _ := e.reg.GetFunction(ta.ID, "hello")
	now := time.Now().UTC().Truncate(time.Second)
	pr.vms[ta.ID] = []pool.VMInfo{{ID: "vm-a", TenantID: ta.ID, FunctionID: f.ID, State: pool.Idle, Since: now, LastUsed: now, Calls: 3}}
	pr.vms[tb.ID] = []pool.VMInfo{{ID: "vm-b", TenantID: tb.ID, FunctionID: 999, State: pool.Busy}}

	l := decode[apitypes.VMList](t, e.do("GET", "/api/v1/vms", alice, nil, ""))
	if len(l.VMs) != 1 || l.VMs[0].ID != "vm-a" || l.VMs[0].Function != "hello" || l.VMs[0].State != "idle" || l.VMs[0].Calls != 3 {
		t.Errorf("alice sees %+v", l.VMs)
	}
	l = decode[apitypes.VMList](t, e.do("GET", "/api/v1/vms", bob, nil, ""))
	if len(l.VMs) != 1 || l.VMs[0].ID != "vm-b" {
		t.Errorf("bob sees %+v", l.VMs)
	}
	status(t, e.do("GET", "/api/v1/vms", "", nil, ""), 401)
}
