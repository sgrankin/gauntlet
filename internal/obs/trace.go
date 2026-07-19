// Package obs is a thin OTel wrapper around the queue's run lifecycle: it
// starts/ends the span tree described in docs/design/core.md
// ("Observability") and maps core.RunRecord / core.CheckResult onto span
// attributes. It depends only on core and the OTel API (no SDK) — with no
// provider registered, every span produced here is a no-op.
package obs

import (
	"context"

	"github.com/sgrankin/gauntlet/internal/core"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Attribute keys used on gauntlet spans.
const (
	AttrRunID         = "gauntlet.run.id"
	AttrTarget        = "gauntlet.target"
	AttrCandidateRef  = "gauntlet.candidate.ref"
	AttrCandidateSHA  = "gauntlet.candidate.sha"
	AttrBaseOID       = "gauntlet.base.oid"
	AttrMergeSHA      = "gauntlet.merge.sha"
	AttrOutcome       = "gauntlet.outcome"
	AttrDetail        = "gauntlet.detail"
	AttrChecksTotal   = "gauntlet.checks.total"
	AttrCheckName     = "gauntlet.check.name"
	AttrCheckStatus   = "gauntlet.check.status"
	AttrCheckDuration = "gauntlet.check.duration_ms"

	// AttrReceiptRef, AttrReceiptBlob, and AttrReceiptOutcome carry the
	// receipt-notes provenance of a run whose note was CONFIRMED PUBLISHED
	// (issue #13, core.RunRecord.ReceiptRef/ReceiptBlob/ReceiptPublished)
	// alongside the existing landing attributes — "" only when the policy
	// is disabled or the spec declared no receipt. NOT landed-only: a run
	// that publishes and then loses the target race (stale CAS, crash)
	// carries these too, by design (RunRecord's own field doc), same as
	// every other RunRecord-derived attribute here.
	AttrReceiptRef     = "gauntlet.receipt.ref"
	AttrReceiptBlob    = "gauntlet.receipt.blob"
	AttrReceiptOutcome = "gauntlet.receipt.outcome"
)

// Tracer returns gauntlet's shared tracer. With no provider registered
// (the default), every span it produces is a no-op
// (IsRecording()==false) and carries no cost.
func Tracer() trace.Tracer { return otel.Tracer("gauntlet") }

// StartRun starts the root "run" span for one run, tagged with the
// candidate identity known at trial-merge time. It returns a context
// carrying the span; callers that fan work out to another goroutine (e.g.
// a check) should derive that goroutine's context from this one via
// trace.ContextWithSpan(ctx, rootSpan) so children parent correctly across
// the goroutine boundary.
func StartRun(ctx context.Context, tr trace.Tracer, runID, target string, cand core.Candidate, mergeSHA string) (context.Context, trace.Span) {
	return tr.Start(ctx, "run", trace.WithAttributes(
		attribute.String(AttrRunID, runID),
		attribute.String(AttrTarget, target),
		attribute.String(AttrCandidateRef, cand.Ref),
		attribute.String(AttrCandidateSHA, cand.SHA),
		attribute.String(AttrMergeSHA, mergeSHA),
	))
}

// StartTrialMerge starts the "trial-merge" child span.
func StartTrialMerge(ctx context.Context, tr trace.Tracer) (context.Context, trace.Span) {
	return tr.Start(ctx, "trial-merge")
}

// StartCheck starts a "check" child span for one named check. ctx must
// carry the run's root span as parent, per StartRun's doc.
func StartCheck(ctx context.Context, tr trace.Tracer, name string) (context.Context, trace.Span) {
	return tr.Start(ctx, "check", trace.WithAttributes(attribute.String(AttrCheckName, name)))
}

// EndCheck records result's per-check attributes on span, sets a status
// derived from result (Ok if passed, Unset if skipped, Error — with
// result.Err or the check name as description — if failed or errored), and
// ends span.
func EndCheck(span trace.Span, result core.CheckResult) {
	span.SetAttributes(checkAttributes(result)...)
	switch {
	case result.Err != nil:
		span.SetStatus(codes.Error, result.Err.Error())
	case result.Status == core.CheckFailed:
		span.SetStatus(codes.Error, result.Name+" failed")
	case result.Status == core.CheckSkipped:
		span.SetStatus(codes.Unset, "")
	default:
		span.SetStatus(codes.Ok, "")
	}
	span.End()
}

// StartLand starts the "land" child span.
func StartLand(ctx context.Context, tr trace.Tracer) (context.Context, trace.Span) {
	return tr.Start(ctx, "land")
}

// EndSpan ends a non-check child span (trial-merge, land): Ok if err is
// nil, Error with err's message otherwise.
func EndSpan(span trace.Span, err error) {
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
	} else {
		span.SetStatus(codes.Ok, "")
	}
	span.End()
}

// EndRun ends the run's root span: records rec's summary as attributes and
// sets a status mapped from rec.Outcome (Landed -> Ok; Rejected, Conflict,
// Error -> Error with rec.Detail as description; Skipped -> Unset).
// rec nil (a run ending without ever producing a record) just ends span.
func EndRun(span trace.Span, rec *core.RunRecord) {
	if rec == nil {
		span.End()
		return
	}
	span.SetAttributes(runAttributes(rec)...)
	code, desc := outcomeStatus(rec.Outcome, rec.Detail)
	span.SetStatus(code, desc)
	span.End()
}

// checkAttributes builds a CheckResult's span attributes: name, status
// (stringified), and duration in milliseconds.
func checkAttributes(r core.CheckResult) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String(AttrCheckName, r.Name),
		attribute.String(AttrCheckStatus, checkStatusString(r.Status)),
		attribute.Int64(AttrCheckDuration, r.Duration.Milliseconds()),
	}
}

// runAttributes builds a RunRecord's summary span attributes.
func runAttributes(rec *core.RunRecord) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String(AttrRunID, rec.RunID),
		attribute.String(AttrTarget, rec.Target),
		attribute.String(AttrCandidateRef, rec.Candidate.Ref),
		attribute.String(AttrCandidateSHA, rec.Candidate.SHA),
		attribute.String(AttrBaseOID, rec.BaseOID),
		attribute.String(AttrMergeSHA, rec.MergeSHA),
		attribute.String(AttrOutcome, outcomeString(rec.Outcome)),
		attribute.Int(AttrChecksTotal, len(rec.Checks)),
		attribute.String(AttrDetail, rec.Detail),
		attribute.String(AttrReceiptRef, rec.ReceiptRef),
		attribute.String(AttrReceiptBlob, rec.ReceiptBlob),
		attribute.String(AttrReceiptOutcome, rec.ReceiptPublished),
	}
}

func checkStatusString(s core.CheckStatus) string {
	switch s {
	case core.CheckPassed:
		return "passed"
	case core.CheckFailed:
		return "failed"
	case core.CheckSkipped:
		return "skipped"
	case core.CheckBlocked:
		return "blocked"
	default:
		return "unknown"
	}
}

func outcomeString(o core.Outcome) string {
	switch o {
	case core.OutcomeLanded:
		return "landed"
	case core.OutcomeRejected:
		return "rejected"
	case core.OutcomeConflict:
		return "conflict"
	case core.OutcomeSkipped:
		return "skipped"
	case core.OutcomeError:
		return "error"
	default:
		return "unknown"
	}
}

// outcomeStatus maps a run Outcome to a span status code and description.
// Only Error carries a description (RunRecord.Detail); Ok and Unset don't
// need one.
func outcomeStatus(o core.Outcome, detail string) (codes.Code, string) {
	switch o {
	case core.OutcomeLanded:
		return codes.Ok, ""
	case core.OutcomeSkipped:
		return codes.Unset, ""
	default: // Rejected, Conflict, Error
		return codes.Error, detail
	}
}
