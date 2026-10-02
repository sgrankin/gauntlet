package review

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// Webhook treats authenticated deliveries as refresh hints. Admission and
// landing still consult GitHub; duplicate or missed deliveries need no replay
// ledger because periodic polling reconstructs the same state.
func (g *GitHub) Webhook(secret string, wake func()) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		if secret == "" {
			http.Error(w, "webhook disabled", http.StatusServiceUnavailable)
			return
		}
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(10 * time.Second))
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
		if err != nil {
			http.Error(w, "invalid or oversized delivery", http.StatusRequestEntityTooLarge)
			return
		}
		signature, err := hex.DecodeString(strings.TrimPrefix(r.Header.Get("X-Hub-Signature-256"), "sha256="))
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		if err != nil || !strings.HasPrefix(r.Header.Get("X-Hub-Signature-256"), "sha256=") || !hmac.Equal(signature, mac.Sum(nil)) {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
		var delivery struct {
			Repository struct {
				FullName string `json:"full_name"`
			}
		}
		if err := json.Unmarshal(body, &delivery); err != nil {
			http.Error(w, "invalid delivery", http.StatusBadRequest)
			return
		}
		if r.Header.Get("X-GitHub-Event") == "ping" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if !strings.EqualFold(delivery.Repository.FullName, g.p.Repo) {
			http.Error(w, "wrong repository", http.StatusBadRequest)
			return
		}
		g.Invalidate()
		if wake != nil {
			wake()
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
