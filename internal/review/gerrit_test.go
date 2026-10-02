package review

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/sgrankin/gauntlet/internal/core"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testGerritChange(t *testing.T) change {
	t.Helper()
	var c change
	if err := json.Unmarshal([]byte(`{"id":"acme~main~Iaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","_number":4,"branch":"main","status":"NEW","subject":"Fix","current_revision":"abc","revisions":{"abc":{"ref":"refs/changes/04/4/1","commit":{"message":"Fix\n\nChange-Id: Iaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n","parents":[{"commit":"base"}]}}},"submit_requirements":[{"name":"Code-Review","status":"SATISFIED"},{"name":"Verified","status":"UNSATISFIED"}]}`), &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestGerritPatchSetVerificationAndRequirements(t *testing.T) {
	c := testGerritChange(t)
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

func TestGerritPostVoteRace(t *testing.T) {
	for _, tc := range []struct {
		name      string
		afterVote func(*change)
		want      bool
	}{
		{"satisfied", func(c *change) { c.Requirements[1].Status = "SATISFIED" }, true},
		{"still unverified", func(*change) {}, false},
		{"new patch set", func(c *change) { c.CurrentRevision = "new" }, false},
		{"review revoked", func(c *change) { c.Requirements[1].Status = "SATISFIED"; c.Requirements[0].Status = "UNSATISFIED" }, false},
		{"abandoned", func(c *change) { c.Status = "ABANDONED" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testGerritChange(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/a/changes/":
					json.NewEncoder(w).Encode([]change{c})
				case "/a/changes/4/detail":
					json.NewEncoder(w).Encode(c)
				case "/a/changes/4/revisions/abc/review":
					if r.Method != "POST" {
						t.Error("vote used wrong method")
					}
					tc.afterVote(&c)
					w.Write([]byte("{}"))
				default:
					t.Error("unexpected request", r.URL)
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			g := NewGerrit(GerritParams{APIURL: srv.URL, Project: "acme", Targets: map[string]string{"main": "main"}, Git: &reviewGit{landed: true}})
			candidates, err := g.Candidates(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(candidates) != 1 {
				t.Fatal(candidates)
			}
			err = g.Validate(context.Background(), candidates[0])
			if (err == nil) != tc.want {
				t.Fatalf("validated=%v, want %v: %v", err == nil, tc.want, err)
			}
		})
	}
}

func TestGerritCompletionRequiresMergedState(t *testing.T) {
	for _, state := range []string{"NEW", "ABANDONED", "MERGED"} {
		t.Run(state, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/a/changes/4/detail" || r.Method != "GET" {
					t.Error("completion changed remote state", r.Method, r.URL)
				}
				json.NewEncoder(w).Encode(map[string]string{"status": state})
			}))
			defer srv.Close()
			g := NewGerrit(GerritParams{APIURL: srv.URL})
			c := core.Candidate{Ref: gerritSlot("main", 4)}
			for range 2 {
				err := g.Landed(context.Background(), c, "tested")
				if (err == nil) != (state == "MERGED") {
					t.Fatal(fmt.Sprint("wrong completion result: ", state, err))
				}
			}
		})
	}
}
