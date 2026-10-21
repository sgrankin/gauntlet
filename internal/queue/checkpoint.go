package queue

import (
	"maps"
	"reflect"
)

func cloneNested[V any](in map[string]map[string]V) map[string]map[string]V {
	out := make(map[string]map[string]V, len(in))
	for key, values := range in {
		out[key] = maps.Clone(values)
	}
	return out
}

// Checkpoint scheduler decisions before admitting work and after terminal
// transitions. Incident controls and scheduling state share one atomic write.
func (d *Daemon) persistScheduler() bool {
	if !d.controls.Uncertain && d.controls.Checkpoint && reflect.DeepEqual(d.order, d.controls.Order) && reflect.DeepEqual(d.done, d.controls.Parks) && reflect.DeepEqual(d.autoRetried, d.controls.AutoRetried) {
		return true
	}
	next := d.controls.clone()
	if next.Uncertain {
		next.LastError = ""
	}
	next.Checkpoint = true
	next.Order = cloneNested(d.order)
	next.Parks = cloneNested(d.done)
	next.AutoRetried = cloneNested(d.autoRetried)
	return d.saveControls(next)
}
