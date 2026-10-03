package config

import (
	"strings"
	"testing"
)

// deploySpecExample is docs/architecture/deployment.md's own deploy graph, with
// the one check ParseChecks requires (a spec with no checks is rejected
// for reasons that predate deploy nodes and are untouched by them).
const deploySpecExample = `
check "test" {
    command "true"
}
deploy "migrate" {
    command "./scripts/migrate"
}
deploy "app1" {
    command "./scripts/deploy" "app1"
    after "migrate"
    executor "deployer"
}
deploy "app2" {
    command "./scripts/deploy" "app2"
    after "migrate"
}
`

func TestParseChecks_DeployExample(t *testing.T) {
	cs, err := ParseChecks([]byte(deploySpecExample))
	if err != nil {
		t.Fatalf("ParseChecks: %v", err)
	}
	if len(cs.Deploys) != 3 {
		t.Fatalf("Deploys = %+v, want 3", cs.Deploys)
	}
	want := []DeployNode{
		{Name: "migrate", Command: []string{"./scripts/migrate"}},
		{Name: "app1", Command: []string{"./scripts/deploy", "app1"}, After: []string{"migrate"}, Executor: "deployer"},
		{Name: "app2", Command: []string{"./scripts/deploy", "app2"}, After: []string{"migrate"}},
	}
	for i, w := range want {
		got := cs.Deploys[i]
		if got.Name != w.Name ||
			strings.Join(got.Command, " ") != strings.Join(w.Command, " ") ||
			strings.Join(got.After, " ") != strings.Join(w.After, " ") ||
			got.Executor != w.Executor {
			t.Errorf("Deploys[%d] = %+v, want %+v", i, got, w)
		}
	}
}

// TestParseChecks_NoDeployNodesIsLegal: unlike checks, an empty deploy
// graph is the ordinary case — most repos never deploy through gauntlet.
func TestParseChecks_NoDeployNodesIsLegal(t *testing.T) {
	cs, err := ParseChecks([]byte("check \"test\" {\n    command \"true\"\n}\n"))
	if err != nil {
		t.Fatalf("ParseChecks: %v", err)
	}
	if len(cs.Deploys) != 0 {
		t.Errorf("Deploys = %+v, want none", cs.Deploys)
	}
}

// TestParseChecks_DeployNameNamespaceIsDisjoint pins the design's
// separate-namespace decision from both sides: a check and a deploy node
// may share a name, and neither graph's `after` can reach into the other.
func TestParseChecks_DeployNameNamespaceIsDisjoint(t *testing.T) {
	t.Run("same name in both graphs is legal", func(t *testing.T) {
		cs, err := ParseChecks([]byte(`
check "migrate" {
    command "true"
}
deploy "migrate" {
    command "./scripts/migrate"
}
`))
		if err != nil {
			t.Fatalf("ParseChecks: %v", err)
		}
		if len(cs.Checks) != 1 || len(cs.Deploys) != 1 {
			t.Fatalf("Checks = %+v, Deploys = %+v, want one of each", cs.Checks, cs.Deploys)
		}
	})

	t.Run("a check cannot depend on a deploy node", func(t *testing.T) {
		_, err := ParseChecks([]byte(`
check "test" {
    command "true"
    after "migrate"
}
deploy "migrate" {
    command "./scripts/migrate"
}
`))
		if err == nil {
			t.Fatal("ParseChecks accepted a check whose after names a deploy node")
		}
		if !strings.Contains(err.Error(), `check "test": after "migrate": no such check declared`) {
			t.Errorf("error = %v, want the ordinary unknown-check message", err)
		}
	})

	t.Run("a deploy node cannot depend on a check", func(t *testing.T) {
		_, err := ParseChecks([]byte(`
check "build" {
    command "true"
}
deploy "app1" {
    command "./scripts/deploy"
    after "build"
}
`))
		if err == nil {
			t.Fatal("ParseChecks accepted a deploy node whose after names a check")
		}
		if !strings.Contains(err.Error(), `deploy "app1": after "build": no such deploy node declared`) {
			t.Errorf("error = %v, want the ordinary unknown-deploy-node message", err)
		}
	})
}

func TestParseChecks_DeployRejects(t *testing.T) {
	const checkPreamble = "check \"test\" {\n    command \"true\"\n}\n"
	tests := []struct {
		name string
		spec string
		want string
	}{
		{
			name: "empty name",
			spec: `
deploy "" {
    command "true"
}
`,
			want: "deploy: name must not be empty",
		},
		{
			name: "empty command",
			spec: `
deploy "app1" {
}
`,
			want: `deploy "app1": command must not be empty`,
		},
		{
			name: "no command node at all",
			spec: `
deploy "app1"
`,
			want: `deploy "app1": command must not be empty`,
		},
		{
			name: "duplicate name",
			spec: `
deploy "app1" {
    command "true"
}
deploy "app1" {
    command "true"
}
`,
			want: `deploy "app1": duplicate`,
		},
		{
			name: "after names an unknown node",
			spec: `
deploy "app1" {
    command "true"
    after "nope"
}
`,
			want: `deploy "app1": after "nope": no such deploy node declared`,
		},
		{
			name: "self after",
			spec: `
deploy "app1" {
    command "true"
    after "app1"
}
`,
			want: "a deploy node cannot depend on itself",
		},
		{
			name: "duplicate after",
			spec: `
deploy "migrate" {
    command "true"
}
deploy "app1" {
    command "true"
    after "migrate" "migrate"
}
`,
			want: `deploy "app1": after "migrate": duplicate`,
		},
		{
			name: "two node cycle",
			spec: `
deploy "a" {
    command "true"
    after "b"
}
deploy "b" {
    command "true"
    after "a"
}
`,
			want: "dependency cycle",
		},
		{
			name: "three node cycle",
			spec: `
deploy "a" {
    command "true"
    after "c"
}
deploy "b" {
    command "true"
    after "a"
}
deploy "c" {
    command "true"
    after "b"
}
`,
			want: "dependency cycle",
		},
		{
			name: "image prefix reserved",
			spec: `
deploy "image:base" {
    command "true"
}
`,
			want: `the "image:" name prefix is reserved`,
		},
		{
			name: "receipt prefix reserved",
			spec: `
deploy "receipt:handoff" {
    command "true"
}
`,
			want: `the "receipt:" name prefix is reserved`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseChecks([]byte(checkPreamble + tt.spec))
			if err == nil {
				t.Fatalf("ParseChecks succeeded, want error containing %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestSelectDeployNodes(t *testing.T) {
	cs, err := ParseChecks([]byte(deploySpecExample))
	if err != nil {
		t.Fatalf("ParseChecks: %v", err)
	}
	names := func(nodes []DeployNode) string {
		out := make([]string, len(nodes))
		for i, n := range nodes {
			out[i] = n.Name
		}
		return strings.Join(out, ",")
	}

	tests := []struct {
		name string
		in   []string
		want string
	}{
		{name: "nil selects the whole graph", in: nil, want: "migrate,app1,app2"},
		{name: "empty selects the whole graph", in: []string{}, want: "migrate,app1,app2"},
		{name: "closure pulls in ancestors", in: []string{"app1"}, want: "migrate,app1"},
		{name: "result is declaration order, not selection order", in: []string{"app2", "migrate"}, want: "migrate,app2"},
		{name: "duplicates are deduped", in: []string{"app1", "app1", "migrate"}, want: "migrate,app1"},
		{name: "a leaf-only selection", in: []string{"migrate"}, want: "migrate"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := cs.SelectDeployNodes(tt.in)
			if err != nil {
				t.Fatalf("SelectDeployNodes(%v): %v", tt.in, err)
			}
			if names(got) != tt.want {
				t.Errorf("SelectDeployNodes(%v) = %s, want %s", tt.in, names(got), tt.want)
			}
		})
	}

	t.Run("unknown name is an error", func(t *testing.T) {
		_, err := cs.SelectDeployNodes([]string{"nope"})
		if err == nil {
			t.Fatal("SelectDeployNodes accepted an undeclared node name")
		}
		if !strings.Contains(err.Error(), `deploy node "nope": no such deploy node declared`) {
			t.Errorf("error = %v", err)
		}
	})

	t.Run("a check name is not a deploy node", func(t *testing.T) {
		if _, err := cs.SelectDeployNodes([]string{"test"}); err == nil {
			t.Fatal("SelectDeployNodes resolved a CHECK name; the namespaces are disjoint")
		}
	})
}
