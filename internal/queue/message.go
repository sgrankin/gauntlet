package queue

import (
	"fmt"
	"strings"
	"text/template"
)

// messageFields are the fields available to the merge-message template
// (docs/plans/phase1.md §3): the candidate's topic/user, its queue-slot ref,
// the run ID assigned to this attempt, and the target name.
type messageFields struct {
	Topic  string
	User   string
	Ref    string
	RunID  string
	Target string
}

// defaultMergeMessageWithUser and defaultMergeMessageNoUser are the built-in
// merge-message subject templates used when the daemon config leaves
// merge-message unset. Per §9.3, the "no user" variant is used whenever
// User=="" (solo setups, or a candidate ref with no user segment) so the
// subject doesn't degrade to a bare "Merge topic ()" — a config-supplied
// custom template is rendered exactly as written, with no such
// substitution, since the operator owns it.
const (
	defaultMergeMessageWithUser = "Merge {{.Topic}} ({{.User}})"
	defaultMergeMessageNoUser   = "Merge {{.Topic}}"
)

// buildMergeMessage renders a merge commit's full message: the templated
// subject line (tmplText, or the built-in default chosen per
// messageFields.User when tmplText is empty), an optional blank-line
// separated body (Config.MergeBody's return, trimmed — phase-4's
// Claude-written summary; "" omits it entirely, the exact prior shape),
// and the Gauntlet-Ref / Gauntlet-Run trailers (docs/plans/phase1.md §3).
func buildMergeMessage(tmplText string, f messageFields, body string) (string, error) {
	subjectTmpl := tmplText
	if subjectTmpl == "" {
		if f.User == "" {
			subjectTmpl = defaultMergeMessageNoUser
		} else {
			subjectTmpl = defaultMergeMessageWithUser
		}
	}

	tmpl, err := template.New("merge-message").Parse(subjectTmpl)
	if err != nil {
		return "", fmt.Errorf("queue: parse merge-message template: %w", err)
	}
	var buf strings.Builder
	if err := tmpl.Execute(&buf, f); err != nil {
		return "", fmt.Errorf("queue: render merge-message template: %w", err)
	}
	subject := strings.TrimRight(buf.String(), "\n")

	var msg strings.Builder
	msg.WriteString(subject)
	msg.WriteString("\n\n")
	if body = strings.TrimSpace(body); body != "" {
		msg.WriteString(body)
		msg.WriteString("\n\n")
	}
	fmt.Fprintf(&msg, "Gauntlet-Ref: %s\nGauntlet-Run: %s\n", f.Ref, f.RunID)
	return msg.String(), nil
}
