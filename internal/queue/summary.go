package queue

import (
	"context"
	"sync"

	"github.com/sgrankin/gauntlet/internal/core"
)

// maxConcurrentMergeBodies bounds how many Config.MergeBody calls
// precomputeMergeBodies runs at once (S6, phase-6 audit synthesis):
// batch mode used to call MergeBody once per chained member, serially,
// inside startBatchRun's chain loop, so a batch of N blocked the single
// reconcile goroutine for up to N*cfg.Summarize.Timeout. Fanning the calls
// out — still each individually best-effort per Config.MergeBody's own
// contract — bounds wall clock to roughly one timeout regardless of N. The
// cap keeps a large batch from opening dozens of simultaneous Messages API
// calls; hand-rolled with a semaphore channel + sync.WaitGroup rather than
// golang.org/x/sync/errgroup, which isn't a go.mod dependency of this
// module (only pulled in transitively) and this phase's ground rules don't
// permit adding one for this.
const maxConcurrentMergeBodies = 4

// mergeBodyRequest is one candidate to summarize, alongside the base OID
// Config.MergeBody should diff/log it against — see precomputeMergeBodies'
// doc for why every request in a batch uses the batch's pre-chain target
// tip rather than the true, chain-accumulated per-link base.
type mergeBodyRequest struct {
	cand core.Candidate
	base string
}

// precomputeMergeBodies calls mergeBody for every req concurrently, capped
// at maxConcurrentMergeBodies in flight, and returns the results keyed by
// each candidate's own SHA. mergeBody == nil (Config.MergeBody disabled) or
// an empty reqs both return a nil map without starting any goroutine —
// buildChainLinkPrecomputed's lookup on a nil map degrades to "" per key,
// identical to MergeBody's own best-effort empty-string contract.
//
// Every req in a batch chain is deliberately summarized against the SAME
// base (startBatchRun passes the batch's pre-chain target tip for every
// member, not each link's own chained, unpushed base): the true per-link
// base for member i>0 is the previous link's merge OID, which only exists
// once that prior link's trial merge has actually run — an inherently
// serial dependency buildChainLink alone can't route around, since building
// it requires the real git.MergeTree call this function is explicitly
// trying to get off the critical path. Using the shared target tip instead
// is not a loss of information for what Config.MergeBody actually reads
// (each candidate's own commits and diffstat, base..cand.SHA): every
// candidate here is an independently authored ref that was never rebased
// onto an earlier chain link, so it shares no commits with the synthetic
// merge OIDs between the target tip and that link's chained base — the
// base..cand.SHA range is identical either way. A member later dropped by
// startBatchRun's spec-change boundary simply leaves its precomputed body
// unused in the returned map; that's harmless, per the same best-effort
// contract Config.MergeBody already documents (a summary that's never
// consulted is no different from one that was never computed).
func precomputeMergeBodies(ctx context.Context, mergeBody func(ctx context.Context, cand core.Candidate, baseOID string) string, reqs []mergeBodyRequest) map[string]string {
	if mergeBody == nil || len(reqs) == 0 {
		return nil
	}

	results := make(map[string]string, len(reqs))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, maxConcurrentMergeBodies)

	for _, req := range reqs {
		wg.Add(1)
		sem <- struct{}{}
		go func(req mergeBodyRequest) {
			defer wg.Done()
			defer func() { <-sem }()
			body := mergeBody(ctx, req.cand, req.base)
			mu.Lock()
			results[req.cand.SHA] = body
			mu.Unlock()
		}(req)
	}
	wg.Wait()

	return results
}
