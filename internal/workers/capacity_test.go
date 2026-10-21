package workers

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
)

type fakeCapacity struct {
	inv     Inventory
	grows   []int
	removed []Node
	lost    bool
}

func (p *fakeCapacity) Inventory(context.Context) (Inventory, error) { return p.inv, nil }
func (p *fakeCapacity) Grow(_ context.Context, inv Inventory, n int) error {
	if inv.Generation != p.inv.Generation || n <= p.inv.Capacity {
		return errors.New("stale growth")
	}
	p.grows = append(p.grows, n)
	for i := p.inv.Capacity; i < n; i++ {
		p.inv.Nodes = append(p.inv.Nodes, Node{ID: fmt.Sprint(i), Incarnation: fmt.Sprint(i, "-vm"), Worker: fmt.Sprint("node-", i)})
	}
	p.inv.Capacity = n
	p.inv.Generation += "+"
	if p.lost {
		p.lost = false
		return errors.New("lost response")
	}
	return nil
}
func (p *fakeCapacity) Remove(_ context.Context, inv Inventory, node Node) error {
	if inv.Generation != p.inv.Generation {
		return errors.New("stale removal")
	}
	for i, n := range p.inv.Nodes {
		if n == node {
			p.inv.Nodes = append(p.inv.Nodes[:i], p.inv.Nodes[i+1:]...)
			p.inv.Capacity--
			p.inv.Generation += "-"
			p.removed = append(p.removed, node)
			return nil
		}
	}
	return errors.New("node changed")
}
func capacityFixture(t *testing.T) (*Broker, *Scaler, *fakeCapacity, *time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	b, err := Open(filepath.Join(t.TempDir(), "workers.db"), time.Minute, 0, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	p := &fakeCapacity{inv: Inventory{Generation: "initial"}}
	pool := Pool{Name: "sql", Provider: p, Worker: workerFixture(), Max: 8, IdleTimeout: time.Minute, StartupTimeout: 5 * time.Minute, PricePerHour: .4, MaxHourlyCost: .8}
	s, err := NewScaler(b, []Pool{pool}, filepath.Join(t.TempDir(), "capacity.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.Now = func() time.Time { return now }
	return b, s, p, &now
}
func tickCapacity(t *testing.T, s *Scaler) {
	t.Helper()
	if err := s.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
}
func TestCapacityScalesFromZeroHonorsCostAndRecoversLostResponse(t *testing.T) {
	b, s, p, _ := capacityFixture(t)
	for i := 0; i < 5; i++ {
		if _, err := b.Submit(t.Context(), fmt.Sprint(i), jobFixture(), requirementsFixture(), 1); err != nil {
			t.Fatal(err)
		}
	}
	p.lost = true
	tickCapacity(t, s)
	if len(p.grows) != 1 || p.grows[0] != 2 {
		t.Fatalf("growth/cost cap: %+v", p.grows)
	}
	restored, err := NewScaler(b, s.Pools, s.Path)
	if err != nil {
		t.Fatal(err)
	}
	restored.Now = s.Now
	tickCapacity(t, restored)
	if len(p.grows) != 1 || restored.state["sql"].Action != nil {
		t.Fatalf("lost response repeated or not adopted: %+v", restored.state)
	}
	status, err := b.Status(t.Context())
	if err != nil || len(status.Pools) != 1 || status.Pools[0].Limit != 2 {
		t.Fatalf("fleet capacity: %+v %v", status, err)
	}
}
func TestCapacityDrainsIdleNodesAndRetainsActiveOrUnacknowledgedWork(t *testing.T) {
	b, s, p, now := capacityFixture(t)
	ctx := t.Context()
	s.Pools[0].Warm = 1
	tickCapacity(t, s)
	tickCapacity(t, s)
	w := workerFixture()
	w.ID = p.inv.Nodes[0].Worker
	if _, err := b.Submit(ctx, "running", jobFixture(), requirementsFixture(), 1); err != nil {
		t.Fatal(err)
	}
	a, err := b.Claim(ctx, w)
	if err != nil || a == nil {
		t.Fatalf("claim: %+v %v", a, err)
	}
	s.Pools[0].Warm = 0
	*now = now.Add(2 * time.Minute)
	tickCapacity(t, s)
	if len(p.removed) != 0 {
		t.Fatal("removed active worker")
	}
	if err = b.Complete(ctx, a.ID, a.Token, EncodeResult(core.CheckResult{Status: core.CheckPassed})); err != nil {
		t.Fatal(err)
	}
	w.Usage = requirementsFixture().Resources
	if _, err = b.Claim(ctx, w); err != nil {
		t.Fatal(err)
	}
	tickCapacity(t, s)
	*now = now.Add(2 * time.Minute)
	if _, err = b.Claim(ctx, w); err != nil {
		t.Fatal(err)
	}
	tickCapacity(t, s)
	if len(p.removed) != 0 {
		t.Fatal("removed worker reporting stopping work")
	}
	w.Usage = Resources{}
	if _, err = b.Claim(ctx, w); err != nil {
		t.Fatal(err)
	}
	tickCapacity(t, s)
	*now = now.Add(time.Minute)
	if _, err = b.Claim(ctx, w); err != nil {
		t.Fatal(err)
	}
	tickCapacity(t, s)
	if len(p.removed) != 1 {
		t.Fatalf("idle scale to zero: %+v", p.removed)
	}
	if next, err := b.Claim(ctx, w); err != nil || next != nil {
		t.Fatalf("drained worker: %+v %v", next, err)
	}
}
func TestCapacityOfflineReplacementRevokesAttemptAndPreservesDemand(t *testing.T) {
	b, s, p, now := capacityFixture(t)
	s.Pools[0].Warm = 1
	tickCapacity(t, s)
	tickCapacity(t, s)
	id, err := b.Submit(t.Context(), "running", jobFixture(), requirementsFixture(), 1)
	if err != nil {
		t.Fatal(err)
	}
	w := workerFixture()
	w.ID = p.inv.Nodes[0].Worker
	a, err := b.Claim(t.Context(), w)
	if err != nil || a == nil {
		t.Fatalf("claim: %+v %v", a, err)
	}
	*now = now.Add(6 * time.Minute)
	tickCapacity(t, s)
	if len(p.removed) != 1 {
		t.Fatalf("offline node not retired: %+v", s.status)
	}
	if err = b.Complete(t.Context(), id, a.Token, EncodeResult(core.CheckResult{Status: core.CheckPassed})); !errors.Is(err, ErrStale) {
		t.Fatalf("old token accepted: %v", err)
	}
	tickCapacity(t, s)
	tickCapacity(t, s)
	if len(p.grows) != 2 {
		t.Fatalf("warm replacement: %+v", p.grows)
	}
	other := workerFixture()
	other.ID = "replacement"
	other.Session = "new"
	next, err := b.Claim(t.Context(), other)
	if err != nil || next == nil || next.ID != id || next.Token == a.Token {
		t.Fatalf("demand lost: %+v %v", next, err)
	}
}
func TestCapacityPackingAssignsOnceAndReservesUrgentResources(t *testing.T) {
	_, s, _, _ := capacityFixture(t)
	p := s.Pools[0]
	p.Worker.UrgentReserve = 1
	ds := []Demand{{Profile: "sql", Requirements: requirementsFixture(), Pending: 3, Priority: 1}}
	if n := s.desired(p, Inventory{}, &ds); n != 2 || ds[0].Pending != 1 {
		t.Fatalf("reserve/cost packing: %d %+v", n, ds)
	}
	if n := s.desired(p, Inventory{}, &ds); n != 1 || ds[0].Pending != 0 {
		t.Fatalf("second pool remaining demand: %d %+v", n, ds)
	}
	ds = []Demand{{Profile: "sql", Requirements: requirementsFixture(), Pending: 3, Priority: 3}}
	if n := s.desired(p, Inventory{}, &ds); n != 2 || ds[0].Pending != 0 {
		t.Fatalf("urgent packing: %d %+v", n, ds)
	}
}
