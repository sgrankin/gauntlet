package gauntlet.defaults
import rego.v1

writer(permission) if permission in {"admin", "maintain", "write"}
principal := object.get(input, "principal", {})
command_facts := object.get(input, "command", {})
settings := object.get(input, "settings", {})
emergency := object.get(input, "emergency", {})
candidate := object.get(input, "candidate", {"source": ""})
forge := object.get(input, "forge", {})

authorized if { principal.authenticated; principal.source == "github"; writer(principal.permission) }
authorized if { principal.authenticated; principal.source in {"admin", "internal"} }
authorized if { principal.authenticated; principal.source == "slack"; principal.allowed_by_config }
normal_request if { not object.get(emergency, "skip_checks", false); not object.get(emergency, "override_pause", false) }
emergency_enabled if normal_request
emergency_enabled if object.get(settings, "emergency_enabled", false)
auth_ok := count([true | authorized]) > 0
emergency_ok := count([true | emergency_enabled]) > 0
command_requirements := [
 {"name":"command-authority", "satisfied":auth_ok, "reason":"Requester is not authorized for this command"},
 {"name":"emergency-enabled", "satisfied":emergency_ok, "reason":"Emergency requests are disabled"},
]
command := {"allow":true,"requirements":command_requirements}

trusted_reviews := [r | some r in forge.reviews; writer(r.permission)]
approvals := [r | some r in trusted_reviews; r.state == "APPROVED"; r.current]
no_veto if not some_veto
some_veto if { some r in trusted_reviews; r.state == "CHANGES_REQUESTED" }
conversations_ok if not object.get(settings,"resolved_conversations",false)
conversations_ok if forge.conversations_resolved == true
check_passed(c) if { c.kind == "status"; c.state == "success" }
check_passed(c) if { c.kind == "check_run"; c.state == "completed"; c.conclusion in {"success","neutral","skipped"} }
check_ok(name) if {
 matches := [c | some c in forge.checks; c.name == name]
 count(matches) > 0
 every c in matches { check_passed(c) }
}
check_ok(name) if object.get(emergency,"skip_checks",false)
veto_ok := count([true | no_veto]) > 0
conversation_ok := count([true | conversations_ok]) > 0
check_satisfied(name) := count([true | check_ok(name)]) > 0
github_requirements := array.concat([
 {"name":"review-open", "satisfied":open_ok, "reason":"PR is closed"},
 {"name":"review-draft", "satisfied":not_draft, "reason":"PR is a draft"},
 {"name":"review-veto", "satisfied":veto_ok, "reason":"Changes requested by a trusted reviewer"},
 {"name":"approvals", "satisfied":approval_ok, "reason":"Not enough current approvals"},
 {"name":"conversations", "satisfied":conversation_ok, "reason":"Unresolved or inaccessible review conversations"},
], [{"name":"required-check", "satisfied":check_satisfied(name), "reason":sprintf("required check is missing or not passing: %s",[name])} | some name in object.get(settings,"required_checks",[])])
not_draft := count([true | forge.draft == false]) > 0
open_ok := count([true | forge.state == "open"]) > 0
approval_ok := count(approvals) >= object.get(settings,"approvals",0)
submission := {"allow":true,"requirements":github_requirements} if { candidate.source == "github"; not object.get(input,"adapter_validated",false) }
submission := {"allow":true,"requirements":[]} if candidate.source != "github"
submission := {"allow":true,"requirements":[]} if object.get(input,"adapter_validated",false)

execution := {"allow":true,"requirements":[]}
deployment := {"allow":true,"requirements":[]}
retry_facts := object.get(input,"retry",{})
retry_ok := count([true | retry_facts.action == "retry"; retry_facts.confidence >= retry_facts.min_confidence]) > 0
retry := {"allow":true,"requirements":[{"name":"retry-confidence","satisfied":retry_ok,"reason":"Retry recommendation does not meet the confidence threshold"}]}

authorized if { principal.authenticated; principal.source == "github"; command_facts.kind == "check" }
