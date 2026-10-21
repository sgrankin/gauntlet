package workers

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/sgrankin/gauntlet/internal/statefile"
	"io"
	"os"
	"slices"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
)

// Providers own dedicated pools. Grow must never reduce capacity; Remove must
// compare immutable node identities. Generation binds mutations to Inventory.
type CapacityProvider interface {
	Inventory(context.Context) (Inventory, error)
	Grow(context.Context, Inventory, int) error
	Remove(context.Context, Inventory, Node) error
}
type Node struct {
	ID          string `json:"id"`
	Incarnation string `json:"incarnation"`
	Worker      string `json:"worker"`
}
type Inventory struct {
	Generation string `json:"generation"`
	Nodes      []Node `json:"nodes"`
	Capacity   int    `json:"capacity"`
	Busy       bool   `json:"busy"`
}
type Pool struct {
	Name                        string
	Provider                    CapacityProvider
	Worker                      Worker
	Warm, Max                   int
	IdleTimeout, StartupTimeout time.Duration
	PricePerHour, MaxHourlyCost float64
}
type PoolStatus struct {
	Name       string  `json:"name"`
	Nodes      int     `json:"nodes"`
	Desired    int     `json:"desired"`
	Limit      int     `json:"limit"`
	HourlyCost float64 `json:"hourlyCost"`
	Action     string  `json:"action,omitempty"`
	Error      string  `json:"error,omitempty"`
}
type nodeClock struct{ First, Idle time.Time }
type poolAction struct {
	Kind    string
	Error   string
	At      time.Time
	Desired int
	Node    Node
}
type poolState struct {
	Nodes  map[string]nodeClock
	Action *poolAction
}
type Scaler struct {
	Broker *Broker
	Pools  []Pool
	Path   string
	Now    func() time.Time
	state  map[string]*poolState
	status []PoolStatus
}

func NewScaler(b *Broker, pools []Pool, path string) (*Scaler, error) {
	s := &Scaler{Broker: b, Pools: pools, Path: path, Now: time.Now, state: map[string]*poolState{}}
	f, err := os.Open(path)
	var data []byte
	if err == nil {
		data, err = io.ReadAll(io.LimitReader(f, (4<<20)+1))
		f.Close()
		if len(data) > 4<<20 {
			return nil, fmt.Errorf("capacity state exceeds limit")
		}
	}
	if err == nil {
		err = json.Unmarshal(data, &s.state)
	}
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("capacity state: %w", err)
	}
	if s.state == nil {
		s.state = map[string]*poolState{}
	}
	return s, nil
}
func (s *Scaler) save() error {
	data, err := json.Marshal(s.state)
	if err != nil {
		return err
	}
	return statefile.Write(s.Path, data)
}
func poolLimit(p Pool) int {
	limit := p.Max
	if p.MaxHourlyCost > 0 {
		if affordable := p.MaxHourlyCost/p.PricePerHour + 1e-9; affordable < float64(limit) {
			limit = int(affordable)
		}
	}
	return max(0, limit)
}
func nodeKey(n Node) string { return n.ID + "/" + n.Incarnation }

// Tick is called serially by the scheduler runtime. Intent is synced before a
// cloud mutation; uncertain responses are reconciled from inventory on restart.
func (s *Scaler) Tick(ctx context.Context) error {
	status, err := s.Broker.Status(ctx)
	if err != nil {
		return err
	}
	defer func() {
		s.Broker.mu.Lock()
		s.Broker.pools = append([]PoolStatus(nil), s.status...)
		s.Broker.mu.Unlock()
	}()
	// Assign each pending job to at most one pool. Running jobs stay with their
	// actual pool; unmatched/manual workers never become deletion candidates.
	demand := append([]Demand(nil), status.Demand...)
	s.status = nil
	for _, p := range s.Pools {
		ps := PoolStatus{Name: p.Name, Limit: poolLimit(p)}
		inv, e := p.Provider.Inventory(ctx)
		if e != nil {
			ps.Error = e.Error()
			s.status = append(s.status, ps)
			continue
		}
		if e = validateInventory(inv); e != nil {
			ps.Error = e.Error()
			s.status = append(s.status, ps)
			continue
		}
		ps.Nodes = inv.Capacity
		ps.HourlyCost = float64(inv.Capacity) * p.PricePerHour
		st := s.state[p.Name]
		if st == nil {
			st = &poolState{Nodes: map[string]nodeClock{}}
			s.state[p.Name] = st
		}
		if st.Nodes == nil {
			st.Nodes = map[string]nodeClock{}
		}
		now := s.Now()
		live := map[string]bool{}
		for _, n := range inv.Nodes {
			key := nodeKey(n)
			live[key] = true
			if _, ok := st.Nodes[key]; !ok {
				st.Nodes[key] = nodeClock{First: now}
			}
		}
		for k := range st.Nodes {
			if !live[k] {
				delete(st.Nodes, k)
			}
		}
		desired := s.desired(p, inv, &demand)
		ps.Desired = desired
		if st.Action != nil {
			a := st.Action
			done := a.Kind == "grow" && inv.Capacity >= a.Desired && len(inv.Nodes) >= a.Desired && !inv.Busy
			if a.Kind == "remove" {
				done = !live[nodeKey(a.Node)] && !inv.Busy
			}
			if done {
				st.Action = nil
			} else {
				ps.Action = a.Kind
				ps.Error = a.Error
				if now.Sub(a.At) > p.StartupTimeout && !inv.Busy {
					// Absolute growth and incarnation-bound removal are idempotent.
					// Retry uncertain requests only after observing a settled pool.
					a.At = now
					if e = s.save(); e != nil {
						return e
					}
					if a.Kind == "grow" {
						if a.Desired <= poolLimit(p) && inv.Capacity < a.Desired {
							e = p.Provider.Grow(ctx, inv, a.Desired)
						} else {
							e = fmt.Errorf("pending growth exceeds current pool limit")
						}
					} else {
						e = p.Provider.Remove(ctx, inv, a.Node)
					}
					a.Error = ""
					if e != nil {
						a.Error = e.Error()
					}
					ps.Error = a.Error
				}
				s.status = append(s.status, ps)
				continue
			}
		}
		if inv.Busy {
			ps.Action = "provisioning"
			s.status = append(s.status, ps)
			continue
		}
		// Drain state survives scheduler restarts. Re-enable only workers owned by
		// this dedicated pool before growth; deletion always drains atomically again.
		if desired >= inv.Capacity {
			for _, n := range inv.Nodes {
				if peer, ok := findPeer(status, n.Worker); ok && peer.Worker.Draining {
					_ = s.Broker.Drain(ctx, n.Worker, false)
				}
			}
		}
		if desired > inv.Capacity {
			st.Action = &poolAction{Kind: "grow", At: now, Desired: desired}
			ps.Action = "grow"
			if e = s.save(); e != nil {
				return e
			}
			e = p.Provider.Grow(ctx, inv, desired)
			if e != nil {
				ps.Error = e.Error()
				st.Action.Error = ps.Error
			} // retain intent: the request may have committed
		} else {
			// Even at the warm floor, replace nodes that never start or remain offline.
			for _, n := range inv.Nodes {
				key := nodeKey(n)
				clock := st.Nodes[key]
				peer, ok := findPeer(status, n.Worker)
				unhealthy := now.Sub(clock.First) >= p.StartupTimeout && (!ok || !peer.Online && now.Sub(peer.Seen) >= p.StartupTimeout)
				idle := ok && peer.Online && peer.Running == 0 && peer.Worker.Usage.Slots == 0 && peer.Worker.Usage.CPU == 0 && peer.Worker.Usage.Memory == 0
				if idle {
					if clock.Idle.IsZero() {
						clock.Idle = now
					}
				} else {
					clock.Idle = time.Time{}
				}
				st.Nodes[key] = clock
				if !unhealthy && !(inv.Capacity > desired && idle && now.Sub(clock.Idle) >= p.IdleTimeout) {
					continue
				}
				safe, e := s.Broker.drainForRemoval(ctx, n.Worker, now.Add(-p.StartupTimeout), unhealthy)
				if e != nil {
					ps.Error = e.Error()
					break
				}
				if !safe {
					continue
				}
				st.Action = &poolAction{Kind: "remove", At: now, Node: n}
				ps.Action = "remove"
				if e = s.save(); e != nil {
					return e
				}
				if e = p.Provider.Remove(ctx, inv, n); e != nil {
					ps.Error = e.Error()
					st.Action.Error = ps.Error
				}
				break
			}
		}
		s.status = append(s.status, ps)
	}
	return s.save()
}
func findPeer(status Status, id string) (Peer, bool) {
	for _, p := range status.Workers {
		if p.Worker.ID == id {
			return p, true
		}
	}
	return Peer{}, false
}
func validateInventory(inv Inventory) error {
	if inv.Generation == "" || inv.Capacity < 0 || inv.Capacity > 100000 || len(inv.Nodes) > 100000 {
		return fmt.Errorf("invalid capacity inventory")
	}
	ids, workers := map[string]bool{}, map[string]bool{}
	for _, n := range inv.Nodes {
		if n.ID == "" || n.Incarnation == "" || n.Worker == "" || ids[n.ID] || workers[n.Worker] {
			return fmt.Errorf("invalid or duplicate capacity node")
		}
		ids[n.ID] = true
		workers[n.Worker] = true
	}
	return nil
}
func (s *Scaler) desired(p Pool, inv Inventory, demand *[]Demand) int {
	limit := poolLimit(p)
	// First-fit packing honors all three resource budgets and urgent reservations.
	var bins []Resources
	add := func(r Resources, priority int) bool {
		available := p.Worker.Capacity
		if priority < 3 {
			available.Slots -= p.Worker.UrgentReserve
			available.CPU -= reservedShare(p.Worker.Capacity.CPU, p.Worker.Capacity.Slots, p.Worker.UrgentReserve)
			available.Memory -= reservedShare(p.Worker.Capacity.Memory, p.Worker.Capacity.Slots, p.Worker.UrgentReserve)
		}
		if !r.fits(available) {
			return false
		}
		for i, b := range bins {
			used := p.Worker.Capacity.subtract(b)
			if r.fits(available.subtract(used)) {
				bins[i] = b.subtract(r)
				return true
			}
		}
		if len(bins) >= limit {
			return false
		}
		bins = append(bins, p.Worker.Capacity.subtract(r))
		return true
	}
	// Reserve running demand before assigning pending work to this pool.
	for _, d := range *demand {
		if slices.ContainsFunc(inv.Nodes, func(n Node) bool { return n.Worker == d.Worker }) {
			for j := 0; j < d.Running; j++ {
				_ = add(d.Requirements.Resources, 3)
			}
		}
	}
	for i := range *demand {
		d := &(*demand)[i]
		job := core.CheckJob{Executor: d.Profile}
		if d.Image {
			job.Image = "image"
		}
		if d.Services {
			job.ServiceNeeds = []string{"service"}
		}
		if !p.Worker.accepts(job, d.Requirements) {
			continue
		}
		for d.Pending > 0 && add(d.Requirements.Resources, d.Priority) {
			d.Pending--
		}
	}
	return min(limit, max(p.Warm, len(bins)))
}
