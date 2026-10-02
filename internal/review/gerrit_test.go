package review

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGerritPatchSetVerificationAndRequirements(t *testing.T) {
	var c change
	if err := json.Unmarshal([]byte(`{"id":"acme~main~Iaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","_number":4,"branch":"main","status":"NEW","subject":"Fix","current_revision":"abc","revisions":{"abc":{"ref":"refs/changes/04/4/1","commit":{"message":"Fix\n\nChange-Id: Iaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n","parents":[{"commit":"base"}]}}},"submit_requirements":[{"name":"Code-Review","status":"SATISFIED"},{"name":"Verified","status":"UNSATISFIED"}]}`), &c); err != nil {
		t.Fatal(err)
	}
	votes := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != "bot" || p != "secret" {
			t.Error("missing Basic authentication")
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/a/changes/" {
			w.Write([]byte(")]}'\n"))
			json.NewEncoder(w).Encode([]change{c})
			return
		}
		if r.URL.Path == "/a/changes/4/detail" {
			w.Write([]byte(")]}'\n"))
			json.NewEncoder(w).Encode(c)
			return
		}
		if r.URL.Path == "/a/changes/4/revisions/abc/review" && r.Method == "POST" {
			var body struct{ Labels map[string]int }
			json.NewDecoder(r.Body).Decode(&body)
			if body.Labels["Verified"] != 1 {
				t.Error("wrong verification vote")
			}
			votes++
			c.Requirements[1].Status = "SATISFIED"
			w.Write([]byte("{}"))
			return
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL)
		http.NotFound(w, r)
	}))
	defer srv.Close()
	g := NewGerrit(GerritParams{APIURL: srv.URL, Project: "acme", Username: "bot", Token: "secret", Targets: map[string]string{"main": "main"}, Git: &reviewGit{landed: true}})
	cs, err := g.Candidates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || cs[0].SHA != "abc" || cs[0].SourceBase != "base" || !strings.Contains(cs[0].Message, "Change-Id: Iaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa") {
		t.Fatalf("bad patch-set candidate %+v", cs)
	}
	if err := g.Validate(context.Background(), cs[0]); err != nil {
		t.Fatal(err)
	}
	if votes != 1 {
		t.Fatalf("votes=%d", votes)
	}
	c.CurrentRevision = "repushed"
	if err := g.Validate(context.Background(), cs[0]); err == nil {
		t.Fatal("repushed patch set accepted")
	}
	if votes != 1 {
		t.Fatal("voted on a superseded patch set")
	}
}
