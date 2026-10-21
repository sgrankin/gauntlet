package dashboard

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"strings"

	"github.com/sgrankin/gauntlet/internal/workers"
)

func WithWorkers(fn func(context.Context) (workers.Status, error)) Option {
	return func(d *dash) { d.workers = fn }
}

func (d *dash) handleAPIWorkers(w http.ResponseWriter, r *http.Request) {
	if d.workers == nil {
		writeJSONError(w, 503, "workers disabled")
		return
	}
	status, err := d.workers(r.Context())
	if err != nil {
		writeJSONError(w, 503, "worker status unavailable")
		return
	}
	writeJSON(w, 200, status)
}

type workerView struct {
	ID, State, Profiles, Labels, Capacity, Running string
	Seen                                           template.HTML
}
type workersPage struct {
	baseData
	Workers []workerView
	Demand  []workers.Demand
	Error   string
}

func (d *dash) handleWorkers(w http.ResponseWriter, r *http.Request) {
	data := workersPage{baseData: d.newBase("Workers", d.snapshot(), true, "workers")}
	if d.workers == nil {
		http.NotFound(w, r)
		return
	}
	status, err := d.workers(r.Context())
	if err != nil {
		data.Error = "Worker status is unavailable."
	} else {
		data.Demand = status.Demand
		for _, peer := range status.Workers {
			state := "ready"
			if !peer.Online {
				state = "unreachable"
			}
			if peer.Worker.Draining {
				state = "draining"
			}
			profiles := append([]string(nil), peer.Worker.Profiles...)
			for i, name := range profiles {
				if name == "" {
					profiles[i] = "(default)"
				}
			}
			data.Workers = append(data.Workers, workerView{ID: peer.Worker.ID, State: state, Profiles: strings.Join(profiles, ", "), Labels: strings.Join(peer.Worker.Labels, ", "), Capacity: fmt.Sprintf("%g cores · %s", float64(peer.Worker.Capacity.CPU)/1000, formatBytes(peer.Worker.Capacity.Memory)), Running: fmt.Sprintf("%d / %d", peer.Running, peer.Worker.Capacity.Slots), Seen: formatTime(peer.Seen)})
		}
	}
	render(w, workersTmpl, data)
}
