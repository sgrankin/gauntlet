package workers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func capacityProgram(t *testing.T, body string) []string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "provider")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0700); err != nil {
		t.Fatal(err)
	}
	return []string{p}
}
func TestCommandCapacityProtocolAndBoundedFailures(t *testing.T) {
	cmd := capacityProgram(t, `read request
case "$request" in
 *'"operation":"inventory"'*) printf '%s\n' '{"version":1,"inventory":{"generation":"v1","nodes":[],"capacity":0,"busy":false}}' ;;
 *'"generation":"v1"'*) printf '%s\n' '{"version":1}' ;;
 *) exit 3 ;;
esac
`)
	p := CommandProvider{Command: cmd}
	inv, err := p.Inventory(t.Context())
	if err != nil || inv.Generation != "v1" {
		t.Fatalf("inventory %+v %v", inv, err)
	}
	if err = p.Grow(t.Context(), inv, 2); err != nil {
		t.Fatal(err)
	}
	if err = p.Remove(t.Context(), inv, Node{ID: "1", Incarnation: "vm1", Worker: "worker1"}); err != nil {
		t.Fatal(err)
	}
	p.Command = capacityProgram(t, `printf '%s' '{"version":1} {"version":1}'`)
	if _, err = p.Inventory(t.Context()); err == nil {
		t.Fatal("accepted trailing object")
	}
}
func TestAzureVMSSConditionalGrowthAndExactDeletion(t *testing.T) {
	const resource = "/subscriptions/s/resourceGroups/g/providers/Microsoft.Compute/virtualMachineScaleSets/sql"
	generation := "v1"
	capacity := 1
	vmid := "vm-one"
	var mutated []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fake-token" || r.URL.Query().Get("api-version") != "2024-03-01" {
			t.Errorf("missing auth/version")
		}
		switch {
		case r.Method == "GET" && r.URL.Path == resource:
			w.Header().Set("ETag", generation)
			fmt.Fprintf(w, `{"sku":{"capacity":%d},"properties":{"orchestrationMode":"Uniform","provisioningState":"Succeeded"}}`, capacity)
		case r.Method == "GET" && r.URL.Path == resource+"/virtualMachines":
			fmt.Fprintf(w, `{"value":[{"instanceId":"0","properties":{"vmId":%q,"provisioningState":"Succeeded","osProfile":{"computerName":"sql000"}}}]}`, vmid)
		case r.Method == "PATCH" && r.URL.Path == resource:
			if r.Header.Get("If-Match") != generation {
				http.Error(w, "conflict", 412)
				return
			}
			var body struct{ SKU struct{ Capacity int } }
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body.SKU.Capacity <= capacity {
				t.Errorf("unsafe growth: %d", body.SKU.Capacity)
			}
			capacity = body.SKU.Capacity
			generation = "v2"
			mutated = append(mutated, "grow")
			w.WriteHeader(202)
		case r.Method == "POST" && r.URL.Path == resource+"/delete":
			var body struct {
				InstanceIDs []string `json:"instanceIds"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if len(body.InstanceIDs) != 1 || body.InstanceIDs[0] != "0" || r.Header.Get("If-Match") != generation {
				t.Error("unsafe deletion")
			}
			mutated = append(mutated, "remove")
			w.WriteHeader(202)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer server.Close()
	p := AzureVMSS{ResourceID: resource, Endpoint: server.URL, CLI: capacityProgram(t, "printf '%s\\n' fake-token\n")}
	inv, err := p.Inventory(t.Context())
	if err != nil || inv.Generation != "v1" || inv.Nodes[0].Worker != "sql000" {
		t.Fatalf("inventory: %+v %v", inv, err)
	}
	if err = p.Grow(t.Context(), inv, 2); err != nil {
		t.Fatal(err)
	}
	if err = p.Grow(t.Context(), inv, 3); err == nil {
		t.Fatal("stale ETag accepted")
	}
	if err = p.Remove(t.Context(), inv, inv.Nodes[0]); err == nil {
		t.Fatal("stale deletion accepted")
	}
	inv, err = p.Inventory(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	vmid = "replacement"
	if err = p.Remove(t.Context(), inv, inv.Nodes[0]); err == nil {
		t.Fatal("replacement deleted")
	}
	vmid = "vm-one"
	if err = p.Remove(t.Context(), inv, inv.Nodes[0]); err != nil {
		t.Fatal(err)
	}
	if len(mutated) != 2 {
		t.Fatalf("mutations: %+v", mutated)
	}
}
