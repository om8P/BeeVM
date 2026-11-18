package api

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/om8P/BeeVM/internal/apitypes"
	"github.com/om8P/BeeVM/internal/snapshot"
)

// fakeSnaps is an in-memory SnapshotStore.
type fakeSnaps struct {
	mu      sync.Mutex
	list    []snapshot.Info
	removed []string
}

func (f *fakeSnaps) List() ([]snapshot.Info, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]snapshot.Info(nil), f.list...), nil
}

func (f *fakeSnaps) Remove(key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, s := range f.list {
		if s.Key == key {
			f.list = append(f.list[:i], f.list[i+1:]...)
			f.removed = append(f.removed, key)
			return nil
		}
	}
	return snapshot.ErrNotFound
}

func runtimeSnap(key, rt string) snapshot.Info {
	in := snapshot.Inputs{MemMiB: 128, VCPUs: 1}
	if rt != "binary" {
		in.Runtime = rt
	}
	return snapshot.Info{Current: true, Snapshot: snapshot.Snapshot{Meta: snapshot.Meta{
		Key: key, Kind: snapshot.KindRuntime, Inputs: in, MemBytes: 128 << 20, BuildMS: 2100,
		CreatedAt: time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC),
	}}}
}

func functionSnap(key string, tenantID, functionID int64) snapshot.Info {
	return snapshot.Info{Current: true, Snapshot: snapshot.Snapshot{Meta: snapshot.Meta{
		Key: key, Kind: snapshot.KindFunction, Inputs: snapshot.Inputs{Runtime: "python", MemMiB: 128, VCPUs: 1},
		Function: &snapshot.FunctionInputs{TenantID: tenantID, FunctionID: functionID}, MemBytes: 128 << 20, BuildMS: 500,
	}}}
}

// snapEnv: tenants alice (with function "hello") and bob, a store with two runtime snapshots,
// alice's function snapshot, one of hers for a removed function, and one of bob's.
func snapEnv(t *testing.T, allowRuntime bool) (*env, *fakeSnaps, string, string) {
	t.Helper()
	fs := &fakeSnaps{}
	fr := &fakeRunner{}
	e := newEnvDeps(t, fr, fr, func(d *Deps) {
		d.Snapshots = fs
		d.AllowRuntimeSnapshotDelete = allowRuntime
	})
	alice, bob := e.tenant("alice"), e.tenant("bob")
	e.deploy(alice, "hello", elf("x"), "").Body.Close()
	ts, _ := e.reg.ListTenants()
	var aliceID, bobID int64
	for _, tn := range ts {
		if tn.Name == "alice" {
			aliceID = tn.ID
		} else {
			bobID = tn.ID
		}
	}
	f, err := e.reg.GetFunction(aliceID, "hello")
	if err != nil {
		t.Fatal(err)
	}
	fs.list = []snapshot.Info{
		runtimeSnap("rt-binary", "binary"), runtimeSnap("rt-python", "python"),
		functionSnap("fn-alice", aliceID, f.ID), functionSnap("fn-alice-old", aliceID, 9999), functionSnap("fn-bob", bobID, 1),
	}
	return e, fs, alice, bob
}

func TestSnapshotListShowsOnlyOwnFunctionSnapshots(t *testing.T) {
	e, _, alice, _ := snapEnv(t, false)
	l := decode[apitypes.SnapshotList](t, e.do("GET", "/api/v1/snapshots", alice, nil, ""))
	if len(l.Runtime) != 2 || l.Runtime[0].Kind != "runtime" || l.Runtime[1].Runtime != "python" {
		t.Errorf("runtime: %+v", l.Runtime)
	}
	if len(l.Function) != 2 {
		t.Fatalf("function: %+v", l.Function)
	}
	byKey := map[string]apitypes.Snapshot{}
	for _, s := range l.Function {
		byKey[s.Key] = s
	}
	if byKey["fn-alice"].Function != "hello" || byKey["fn-alice"].Kind != "function" || byKey["fn-alice"].SizeBytes != 128<<20 {
		t.Errorf("own snapshot: %+v", byKey["fn-alice"])
	}
	if _, ok := byKey["fn-alice-old"]; !ok || byKey["fn-alice-old"].Function != "" {
		t.Errorf("snapshot of a removed function: %+v", byKey["fn-alice-old"])
	}
	if _, ok := byKey["fn-bob"]; ok {
		t.Error("another tenant's snapshot is listed")
	}
	if l.RuntimeDeleteAllowed {
		t.Error("runtime delete should be off by default")
	}
}

func TestSnapshotDeleteRules(t *testing.T) {
	e, fs, alice, bob := snapEnv(t, false)
	status(t, e.do("DELETE", "/api/v1/snapshots/fn-alice", alice, nil, ""), 204)
	status(t, e.do("DELETE", "/api/v1/snapshots/fn-alice", alice, nil, ""), 404) // gone now
	status(t, e.do("DELETE", "/api/v1/snapshots/fn-bob", alice, nil, ""), 404)   // never 403: not even confirmed
	status(t, e.do("DELETE", "/api/v1/snapshots/nope", alice, nil, ""), 404)
	resp := e.do("DELETE", "/api/v1/snapshots/rt-python", bob, nil, "")
	if resp.StatusCode != 403 {
		t.Fatalf("runtime delete without the flag: %d", resp.StatusCode)
	}
	if msg := decode[apitypes.Error](t, resp).Error; !strings.Contains(msg, "--allow-runtime-snapshot-delete") {
		t.Errorf("403 should say how to allow it: %q", msg)
	}
	status(t, e.do("DELETE", "/api/v1/snapshots/fn-alice-old", alice, nil, ""), 204)
	status(t, e.do("DELETE", "/api/v1/snapshots/fn-bob", "", nil, ""), 401)
	if got := strings.Join(fs.removed, ","); got != "fn-alice,fn-alice-old" {
		t.Errorf("removed: %s", got)
	}
}

func TestSnapshotRuntimeDeleteWhenAllowed(t *testing.T) {
	e, fs, _, bob := snapEnv(t, true)
	l := decode[apitypes.SnapshotList](t, e.do("GET", "/api/v1/snapshots", bob, nil, ""))
	if !l.RuntimeDeleteAllowed {
		t.Error("the list should say runtime delete is allowed")
	}
	status(t, e.do("DELETE", "/api/v1/snapshots/rt-python", bob, nil, ""), 204)
	status(t, e.do("DELETE", "/api/v1/snapshots/fn-alice", bob, nil, ""), 404) // still only own function snapshots
	if strings.Join(fs.removed, ",") != "rt-python" {
		t.Errorf("removed: %v", fs.removed)
	}
}

func TestSnapshotRoutesWithoutSnapshots(t *testing.T) {
	e := newEnv(t) // no SnapshotStore: a --runner cold server
	key := e.tenant("alice")
	status(t, e.do("GET", "/api/v1/snapshots", key, nil, ""), 501)
	status(t, e.do("DELETE", "/api/v1/snapshots/x", key, nil, ""), 501)
}
