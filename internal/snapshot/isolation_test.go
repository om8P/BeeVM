package snapshot

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/om8P/BeeVM/internal/vmm"
)

type nameOnlyIsolator struct{}

func (nameOnlyIsolator) Prepare(*vmm.Config) (func(), error) { return func() {}, nil }
func (nameOnlyIsolator) Name() string                        { return "jailer-test" }

// isolatedTwin is a second Manager on the same folder whose VMs are isolated.
func isolatedTwin(t *testing.T, m *Manager) *Manager {
	t.Helper()
	base := m.base
	base.Isolator = nameOnlyIsolator{}
	m2, err := New(m.Dir(), base, nil)
	if err != nil {
		t.Fatal(err)
	}
	m2.fcVersion, m2.build = m.fcVersion, m.build
	return m2
}

func TestIsolationIsPartOfTheKey(t *testing.T) {
	m, _ := newTestManager(t)
	plain, err := m.Inputs(128, 1)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(plain); strings.Contains(string(b), "isolation") {
		t.Errorf("without isolation the inputs (and so all old keys) must not change: %s", b)
	}
	iso, err := isolatedTwin(t, m).Inputs(128, 1)
	if err != nil {
		t.Fatal(err)
	}
	if iso.Isolation != "jailer-test" || Key(iso) == Key(plain) {
		t.Errorf("an isolated snapshot needs its own key: %+v", iso)
	}
}

func TestEachModeKeepsTheOthersSnapshots(t *testing.T) {
	m, builds := newTestManager(t)
	m2 := isolatedTwin(t, m)
	ctx := context.Background()
	a, err := m.Ensure(ctx, 128, 1)
	if err != nil {
		t.Fatal(err)
	}
	b, err := m2.Ensure(ctx, 128, 1)
	if err != nil {
		t.Fatal(err)
	}
	if a.Key == b.Key || builds.Load() != 2 {
		t.Fatalf("keys %s %s, builds %d", a.Key, b.Key, builds.Load())
	}
	for _, mgr := range []*Manager{m, m2} {
		if n, err := mgr.RemoveStale(); err != nil || n != 0 {
			t.Errorf("RemoveStale removed %d (err %v); both snapshots are current in their own mode", n, err)
		}
	}
	list, _ := m.List()
	if len(list) != 2 {
		t.Fatalf("listed %d snapshots, want 2", len(list))
	}
	for _, s := range list {
		if !s.Current {
			t.Errorf("snapshot %s (isolation %q) listed as stale", s.Key, s.Inputs.Isolation)
		}
	}
}
