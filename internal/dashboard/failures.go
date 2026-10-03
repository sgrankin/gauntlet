package dashboard

import (
	"github.com/sgrankin/gauntlet/internal/flaky"
	"net/http"
)

func WithFailureHistory(h *flaky.History) Option { return func(d *dash) { d.failureHistory = h } }
func (d *dash) handleFailureHistory(w http.ResponseWriter, r *http.Request) {
	if d.failureHistory == nil {
		writeJSONError(w, 503, "failure review history disabled")
		return
	}
	check := r.URL.Query().Get("check")
	if check == "" || len(check) > 256 {
		writeJSONError(w, 400, "check name required")
		return
	}
	observations, err := d.failureHistory.Recent(r.Context(), check)
	if err != nil {
		writeJSONError(w, 500, "failure history unavailable")
		return
	}
	retries, passed := 0, 0
	for _, o := range observations {
		if o.Retried {
			retries++
			if o.Passed {
				passed++
			}
		}
	}
	writeJSON(w, 200, map[string]any{"observations": observations, "observedRetries": retries, "observedRetryPasses": passed, "sampleLimit": 20})
}

type failuresData struct {
	baseData
	Check           string
	Observations    []flaky.Observation
	Retries, Passes int
}

func (d *dash) handleFailures(w http.ResponseWriter, r *http.Request) {
	if d.failureHistory == nil {
		http.Error(w, "failure review history disabled", 503)
		return
	}
	check := r.URL.Query().Get("check")
	if check == "" || len(check) > 256 {
		http.Error(w, "check name required", 400)
		return
	}
	observations, err := d.failureHistory.Recent(r.Context(), check)
	if err != nil {
		http.Error(w, "failure history unavailable", 500)
		return
	}
	data := failuresData{baseData: d.newBase("Failure history", nil, false, "checks"), Check: check, Observations: observations}
	for _, o := range observations {
		if o.Retried {
			data.Retries++
			if o.Passed {
				data.Passes++
			}
		}
	}
	render(w, failuresTmpl, data)
}
