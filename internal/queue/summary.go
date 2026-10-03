package queue

import (
	"context"
	"sync"

	"github.com/sgrankin/gauntlet/internal/core"
)

// Bound concurrent model processes while the reconcile loop builds a batch.
const maxConcurrentMergeBodies = 4

type mergeBodyRequest struct {
	cand core.Candidate
	base string
}

// Summaries use the batch's target tip because synthetic chain bases do not
// exist yet. Key by ref: distinct submissions may share a SHA but have
// different topics or authors. Bodies past a check-spec boundary go unused.
func precomputeMergeBodies(ctx context.Context, mergeBody func(ctx context.Context, cand core.Candidate, baseOID string) string, reqs []mergeBodyRequest) map[string]string {
	if mergeBody == nil || len(reqs) == 0 {
		return nil
	}

	bodies := make([]string, len(reqs))
	var wg sync.WaitGroup
	sem := make(chan struct{}, maxConcurrentMergeBodies)

	for i, req := range reqs {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			bodies[i] = mergeBody(ctx, req.cand, req.base)
		})
	}
	wg.Wait()

	results := make(map[string]string, len(reqs))
	for i, req := range reqs {
		results[req.cand.Ref] = bodies[i]
	}
	return results
}
