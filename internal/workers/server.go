package workers

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/gitx"
	"github.com/sgrankin/gauntlet/internal/testreport"
)

const maxResultBytes = 16 << 20
const maxLogBytes = 256 << 20

type Server struct {
	Broker  *Broker
	Objects *gitx.Repo
	Token   string
	LogDir  string
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/claim", s.claim)
	mux.HandleFunc("POST /v1/attempt/{id}/renew", s.renew)
	mux.HandleFunc("POST /v1/attempt/{id}/source", s.source)
	mux.HandleFunc("PUT /v1/attempt/{id}/log", s.log)
	mux.HandleFunc("POST /v1/attempt/{id}/result", s.complete)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		supplied := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if s.Token == "" || subtle.ConstantTimeCompare([]byte(supplied), []byte(s.Token)) != 1 {
			http.Error(w, "worker authentication required", 401)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		mux.ServeHTTP(w, r)
	})
}

func decode(w http.ResponseWriter, r *http.Request, value any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxResultBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		http.Error(w, "invalid worker request", 400)
		return false
	}
	if decoder.Decode(new(any)) != io.EOF {
		http.Error(w, "one worker request required", 400)
		return false
	}
	return true
}
func reply(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}
func workerError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrStale) {
		http.Error(w, err.Error(), 409)
	} else {
		http.Error(w, "worker operation failed", 500)
	}
}

func (s *Server) claim(w http.ResponseWriter, r *http.Request) {
	var worker Worker
	if !decode(w, r, &worker) {
		return
	}
	attempt, err := s.Broker.Claim(r.Context(), worker)
	if err != nil {
		workerError(w, err)
		return
	}
	if attempt == nil {
		w.WriteHeader(204)
		return
	}
	attempt.Job.Dir, attempt.Job.GitDir, attempt.Job.LogPath = "", "", ""
	reply(w, 200, attempt)
}
func (s *Server) renew(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Token string `json:"token"`
	}
	if !decode(w, r, &request) {
		return
	}
	if err := s.Broker.Renew(r.Context(), r.PathValue("id"), request.Token); err != nil {
		workerError(w, err)
		return
	}
	w.WriteHeader(204)
}

func (b *Broker) authorized(ctx context.Context, id, token string) (Attempt, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var attempt Attempt
	var data, needs []byte
	var state, stored string
	err := b.db.QueryRowContext(ctx, `SELECT job,needs,state,token,worker FROM worker_jobs WHERE id=?`, id).Scan(&data, &needs, &state, &stored, &attempt.Worker)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return attempt, ErrStale
		}
		return attempt, err
	}
	if token == "" || token != stored || state != "leased" {
		return attempt, ErrStale
	}
	attempt.ID, attempt.Token = id, stored
	if err = json.Unmarshal(data, &attempt.Job); err != nil {
		return attempt, err
	}
	err = json.Unmarshal(needs, &attempt.Requirements)
	return attempt, err
}

func sourceCommits(job core.CheckJob) []string {
	var out []string
	seen := map[string]bool{}
	add := func(oid string) {
		if oid != "" && !seen[oid] {
			seen[oid] = true
			out = append(out, oid)
		}
	}
	add(job.MergeSHA)
	add(job.BaseSHA)
	add(job.Candidate.SHA)
	add(job.Candidate.SourceBase)
	for _, c := range job.Candidates {
		add(c.SHA)
		add(c.SourceBase)
	}
	return out
}

func (s *Server) source(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Token string   `json:"token"`
		Haves []string `json:"haves"`
	}
	if !decode(w, r, &request) {
		return
	}
	attempt, err := s.Broker.authorized(r.Context(), r.PathValue("id"), request.Token)
	if err != nil {
		workerError(w, err)
		return
	}
	if len(request.Haves) > 128 || s.Objects == nil {
		http.Error(w, "invalid source request", 400)
		return
	}
	w.Header().Set("Content-Type", "application/x-git-packed-objects")
	// A failed stream is rejected by index-pack at the worker, never treated as
	// a partial usable checkout. Commit hashes bind all received source bytes.
	if err := s.Objects.WritePack(r.Context(), w, sourceCommits(attempt.Job), request.Haves); err != nil {
		panic(http.ErrAbortHandler)
	}
}

func (s *Server) log(w http.ResponseWriter, r *http.Request) {
	token := r.Header.Get("X-Attempt-Token")
	attempt, err := s.Broker.authorized(r.Context(), r.PathValue("id"), token)
	if err != nil {
		workerError(w, err)
		return
	}
	if err = os.MkdirAll(filepath.Dir(s.logPath(attempt)), 0700); err != nil {
		workerError(w, err)
		return
	}
	file, err := os.CreateTemp(filepath.Dir(s.logPath(attempt)), ".worker-log-")
	if err != nil {
		workerError(w, err)
		return
	}
	defer os.Remove(file.Name())
	defer file.Close()
	digest := sha256.New()
	count, err := io.Copy(io.MultiWriter(file, digest), io.LimitReader(r.Body, maxLogBytes+1))
	if err != nil || count > maxLogBytes {
		http.Error(w, "worker log exceeds limit or upload failed", 413)
		return
	}
	if err = file.Sync(); err != nil {
		workerError(w, err)
		return
	}
	path := s.logPath(attempt)
	if err = os.Rename(file.Name(), path); err != nil {
		workerError(w, err)
		return
	}
	reply(w, 200, map[string]string{"digest": hex.EncodeToString(digest.Sum(nil))})
}

func (s *Server) complete(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Token  string `json:"token"`
		Result Result `json:"result"`
	}
	if !decode(w, r, &request) {
		return
	}
	attempt, err := s.Broker.authorized(r.Context(), r.PathValue("id"), request.Token)
	if errors.Is(err, ErrStale) {
		// Completed uploads may be retried after losing their HTTP acknowledgement.
		if err = s.Broker.Complete(r.Context(), r.PathValue("id"), request.Token, request.Result); err == nil {
			w.WriteHeader(204)
			return
		}
	}
	if err != nil {
		workerError(w, err)
		return
	}
	result := request.Result
	result.Check.Name = attempt.Job.Name
	result.Check.Command = attempt.Job.Command
	result.Check.ExecutedRun, result.Check.ExecutedSHA = attempt.Job.RunID, attempt.Job.MergeSHA
	result.Check.ExecutionID, result.Check.Worker = attempt.ID, attempt.Worker
	result.Check.LogPath = ""
	if len(result.Check.Output) > 65536 || result.Check.Status < core.CheckFailed || result.Check.Status > core.CheckWaived || result.Check.Status == core.CheckWaived || result.Check.Status == core.CheckBlocked {
		http.Error(w, "invalid execution verdict", 400)
		return
	}
	if result.Error == "" && result.Check.Status == core.CheckPassed && attempt.Job.TestResults != "" {
		if err := testreport.Validate(result.Check.Tests); err != nil || len(testreport.Failed(result.Check.Tests)) > 0 {
			http.Error(w, "passing result lacks passing test coverage", 400)
			return
		}
	}
	if result.LogDigest != "" {
		if len(result.LogDigest) != 64 {
			http.Error(w, "invalid log digest", 400)
			return
		}
		path := s.logPath(attempt)
		file, err := os.Open(path)
		if err != nil {
			workerError(w, err)
			return
		}
		digest := sha256.New()
		_, err = io.Copy(digest, file)
		file.Close()
		if err != nil || fmt.Sprintf("%x", digest.Sum(nil)) != result.LogDigest {
			http.Error(w, "uploaded log digest mismatch", 409)
			return
		}
		result.Check.LogPath = path
		result.AttemptLogPath = path
	}
	if result.Check.LogPath != "" {
		combined, err := s.combineLogs(r.Context(), attempt.Job.PreviousExecution, result.Check.LogPath)
		if err != nil {
			result.Check.LogPath = ""
			logNote(&result, err)
		} else {
			result.Check.LogPath = combined
		}
	}
	if err = s.Broker.Complete(r.Context(), attempt.ID, request.Token, result); err != nil {
		workerError(w, err)
		return
	}
	w.WriteHeader(204)
}

func (s *Server) logPath(attempt Attempt) string {
	return filepath.Join(s.LogDir, core.SanitizeName(attempt.Job.RunID), attempt.ID+"-"+attempt.Token+".log.zst")
}
