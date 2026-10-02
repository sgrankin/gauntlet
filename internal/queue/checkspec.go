package queue

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/obs"
)

// SpecRejectReason reports capability or receipt-policy mismatches; empty
// means accepted. Run construction and the validate command share this gate.
func SpecRejectReason(spec *config.CheckSpec, hasServices bool, known, imageCapable func(string) bool, receiptPolicy bool) string {
	if spec.RequiresServices() && !hasServices {
		return "check spec declares services but this daemon has no services block"
	}
	if chk, prof := unknownExecutorProfile(spec, known); prof != "" {
		return fmt.Sprintf("check spec: check %q selects unknown executor profile %q", chk, prof)
	}
	if chk, img := imageOnIncapableProfile(spec, imageCapable); img != "" {
		return fmt.Sprintf("check spec: check %q runs candidate-built image %q but its executor profile is not a container profile", chk, img)
	}
	switch rcp := spec.Receipt(); {
	case receiptPolicy && rcp == nil:
		return "this daemon requires a receipt (receipt-notes is configured) but the check spec declares none"
	case !receiptPolicy && rcp != nil:
		return fmt.Sprintf("check spec declares receipt %q but this daemon has no receipt-notes policy", rcp.Name)
	}
	return ""
}

// unknownExecutorProfile returns the first check (spec order) whose
// Executor selection doesn't resolve against known, with the offending
// profile name; ("", "") when every selection resolves. known == nil means
// the daemon defines no named profiles, so any non-empty selection is
// unknown.
func unknownExecutorProfile(spec *config.CheckSpec, known func(string) bool) (check, profile string) {
	for _, c := range spec.Checks {
		if c.Executor == "" {
			continue
		}
		if known == nil || !known(c.Executor) {
			return c.Name, c.Executor
		}
	}
	// Image BUILD commands select profiles too (config.Image.Executor) —
	// same vocabulary, same gate; the offender is reported under its
	// node name so the message matches what history/events would show.
	for _, img := range spec.Images {
		if img.Executor == "" {
			continue
		}
		if known == nil || !known(img.Executor) {
			return imageNodePrefix + img.Name, img.Executor
		}
	}
	// The receipt node (issue #13) selects a profile too, same vocabulary
	// and gate — reported under its own reserved node-name prefix,
	// mirroring the image-build case just above.
	if rcp := spec.Receipt(); rcp != nil && rcp.Executor != "" {
		if known == nil || !known(rcp.Executor) {
			return receiptNodePrefix + rcp.Name, rcp.Executor
		}
	}
	return "", ""
}

// imageNodePrefix is the reserved node-name prefix image builds occupy in
// a run's dependency graph (config.ParseChecks rejects check names under
// it, so the two row kinds can never alias).
const imageNodePrefix = "image:"

// receiptNodePrefix reserves receipt names in the run's dependency graph.
const receiptNodePrefix = "receipt:"

// imageNodeName reports whether node is an image-build node, and for
// which declared image.
func imageNodeName(node string) (image string, ok bool) {
	return strings.CutPrefix(node, imageNodePrefix)
}

// receiptNodeName reports whether node is the receipt node, and its
// declared name — mirroring imageNodeName above.
func receiptNodeName(node string) (name string, ok bool) {
	return strings.CutPrefix(node, receiptNodePrefix)
}

// nodeKind classifies node (a check name, possibly "image:<name>" or
// "receipt:<name>") into the coarse kind obs.RecordNode (issue #14) uses
// as a low-cardinality metric attribute — mirrors imageNodeName/
// receiptNodeName's own prefix stripping so the two classifications can
// never drift apart.
func nodeKind(node string) string {
	if _, ok := imageNodeName(node); ok {
		return obs.NodeKindImage
	}
	if _, ok := receiptNodeName(node); ok {
		return obs.NodeKindReceipt
	}
	return obs.NodeKindCheck
}

// imageOnIncapableProfile returns the first check (spec order) that names
// a candidate-built image but runs on a profile that cannot swap its
// rootfs — a non-container profile (Config.ImageCapableProfile). ("", "")
// when every image consumer is capable.
func imageOnIncapableProfile(spec *config.CheckSpec, capable func(string) bool) (check, image string) {
	for _, c := range spec.Checks {
		if c.Image == "" {
			continue
		}
		if capable == nil || !capable(c.Executor) {
			return c.Name, c.Image
		}
	}
	// The receipt node (issue #13) can consume a candidate-built image
	// too, same contract and gate as a check's Image — reported under its
	// own reserved node-name prefix, mirroring unknownExecutorProfile
	// above.
	if rcp := spec.Receipt(); rcp != nil && rcp.Image != "" {
		if capable == nil || !capable(rcp.Executor) {
			return receiptNodePrefix + rcp.Name, rcp.Image
		}
	}
	return "", ""
}

// buildRunNodes adds image-build nodes and their implicit consumer edges,
// then checks and the optional receipt. Every node uses the same scheduler.
func buildRunNodes(spec *config.CheckSpec) []config.Check {
	rcp := spec.Receipt()
	if len(spec.Images) == 0 && rcp == nil {
		return spec.Checks
	}
	nodes := make([]config.Check, 0, len(spec.Images)+len(spec.Checks)+1)
	for _, img := range spec.Images {
		nodes = append(nodes, config.Check{
			Name:     imageNodePrefix + img.Name,
			Command:  img.Command,
			Executor: img.Executor,
		})
	}
	for _, c := range spec.Checks {
		if c.Image != "" {
			// Clone the edge slice: c is a copy but After's backing array
			// is shared with the parsed spec.
			c.After = append(append([]string(nil), c.After...), imageNodePrefix+c.Image)
		}
		nodes = append(nodes, c)
	}
	if rcp != nil {
		node := config.Check{
			Name:     receiptNodePrefix + rcp.Name,
			Command:  rcp.Command,
			Executor: rcp.Executor,
			Image:    rcp.Image,
			After:    append([]string(nil), rcp.After...),
		}
		if rcp.Image != "" {
			node.After = append(node.After, imageNodePrefix+rcp.Image)
		}
		nodes = append(nodes, node)
	}
	return nodes
}

// validImageRef validates an image build's captured result: exactly one
// IMMUTABLE reference — a local image ID (docker buildx --iidfile output)
// or a digest-pinned registry reference. A mutable tag is rejected
// outright: "same configuration, different bytes" races are the exact
// failure this feature exists to close, so accepting a tag and resolving
// it later would re-open the time-of-check/time-of-use hole.
func validImageRef(raw string) (string, error) {
	ref := strings.TrimSpace(raw)
	if ref == "" {
		return "", fmt.Errorf("build wrote no image reference to $%s (e.g. docker buildx --iidfile \"$%s\")", core.EnvImageResultFile, core.EnvImageResultFile)
	}
	if strings.ContainsAny(ref, " \t\n\r") {
		return "", fmt.Errorf("image result must be exactly one reference, got %q", truncateForError(ref))
	}
	if localImageIDPattern.MatchString(ref) || digestRefPattern.MatchString(ref) {
		return ref, nil
	}
	return "", fmt.Errorf("image result %q is not immutable: need a local image ID (sha256:<64 hex>) or a digest-pinned reference (<repo>@sha256:<64 hex>), never a tag", truncateForError(ref))
}

// validReceipt validates a receipt node's captured result (issue #13):
// raw is exactly what the executor read back from the result file
// (core.CheckResult.Receipt) — nil when the file could not be read at all
// (distinct from a successfully-read empty file, a non-nil zero-length
// slice; see CheckResult.Receipt's doc), maxBytes<=0 means no configured
// ceiling to check against (defensive; SpecRejectReason's symmetric gate
// means a receipt node only ever runs with a non-nil Config.ReceiptNotes,
// so this is normally always positive by the time a receipt node exists at
// all). The executor bounds its own read to maxBytes+1 (CheckJob.
// ReceiptMaxBytes), so len(raw) > maxBytes is exactly the oversize signal,
// never a false positive from a legitimately maxBytes-sized payload.
func validReceipt(raw []byte, maxBytes int) ([]byte, error) {
	if raw == nil {
		return nil, fmt.Errorf("receipt result file could not be read (missing, or left unwritten/unreadable by the command)")
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("receipt result file is empty")
	}
	if maxBytes > 0 && len(raw) > maxBytes {
		return nil, fmt.Errorf("receipt result exceeds the configured max-bytes %d (got at least %d bytes)", maxBytes, len(raw))
	}
	return raw, nil
}

// truncateForError bounds captured result content quoted into an error
// message: the executors already cap the read (executor.
// maxImageResultBytes), and this keeps the message itself readable — a
// misdirected build log should yield a pointed one-line diagnosis, not a
// wall of quoted output on every channel.
func truncateForError(s string) string {
	const cap = 200
	if len(s) <= cap {
		return s
	}
	return s[:cap] + "..."
}

var (
	localImageIDPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	digestRefPattern    = regexp.MustCompile(`^[a-zA-Z0-9._/:-]+@sha256:[0-9a-f]{64}$`)
)

// effectiveMaxParallel normalizes a spec's max-parallel for run
// construction: zero (unset) is the serial default of 1, per
// config.CheckSpec.MaxParallel's contract, so schedulers never
// re-interpret the default.
func effectiveMaxParallel(spec *config.CheckSpec) int {
	if spec.MaxParallel <= 0 {
		return 1
	}
	return spec.MaxParallel
}
