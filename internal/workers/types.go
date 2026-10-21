// Package workers implements durable pull-based execution for one recoverable scheduler.
package workers

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"slices"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
)

type Resources struct {
	Slots  int   `json:"slots"`
	CPU    int64 `json:"cpu"`    // millicores
	Memory int64 `json:"memory"` // bytes
}

func (r Resources) fits(available Resources) bool {
	return r.Slots <= available.Slots && r.CPU <= available.CPU && r.Memory <= available.Memory
}
func (r Resources) subtract(used Resources) Resources {
	return Resources{r.Slots - used.Slots, r.CPU - used.CPU, r.Memory - used.Memory}
}

type Requirements struct {
	Labels    []string  `json:"labels"`
	Resources Resources `json:"resources"`
}

type Worker struct {
	ImageProfiles   []string  `json:"imageProfiles"`
	ServiceProfiles []string  `json:"serviceProfiles"`
	ID              string    `json:"id"`
	Session         string    `json:"session"`
	Profiles        []string  `json:"profiles"`
	Labels          []string  `json:"labels"`
	Capacity        Resources `json:"capacity"`
	Usage           Resources `json:"usage"`
	UrgentReserve   int       `json:"urgentReserve"`
	Draining        bool      `json:"draining"`
}

func (w Worker) accepts(job core.CheckJob, needs Requirements) bool {
	if job.Image != "" && !slices.Contains(w.ImageProfiles, job.Executor) {
		return false
	}
	if len(job.ServiceNeeds) > 0 && !slices.Contains(w.ServiceProfiles, job.Executor) {
		return false
	}
	if !slices.Contains(w.Profiles, job.Executor) {
		return false
	}
	for _, label := range needs.Labels {
		if !slices.Contains(w.Labels, label) {
			return false
		}
	}
	return true
}

type Attempt struct {
	ID           string        `json:"id"`
	Token        string        `json:"token"`
	Worker       string        `json:"worker"`
	LeaseUntil   time.Time     `json:"leaseUntil"`
	LeaseMillis  int64         `json:"leaseMillis"`
	Job          core.CheckJob `json:"job"`
	Requirements Requirements  `json:"requirements"`
}

type Result struct {
	AttemptLogPath string           `json:"-"`
	Check          core.CheckResult `json:"check"`
	Error          string           `json:"error,omitempty"`
	LogDigest      string           `json:"logDigest,omitempty"`
}

func EncodeResult(check core.CheckResult) Result {
	result := Result{Check: check}
	if check.Err != nil {
		result.Error = check.Err.Error()
		result.Check.Err = nil
	}
	return result
}

func (r Result) CheckResult() core.CheckResult {
	check := r.Check
	if r.Error != "" {
		check.Err = fmt.Errorf("worker: %s", r.Error)
	}
	return check
}

func randomID() string {
	var value [24]byte
	if _, err := rand.Read(value[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(value[:])
}

func reservedShare(capacity int64, slots, reserve int) int64 {
	divisor := int64(slots)
	count := int64(reserve)
	return capacity/divisor*count + (capacity%divisor*count+divisor-1)/divisor
}
