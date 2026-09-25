package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/krelinga/claude-spool/backend/internal/auth"
)

// AuthManager is the slice of the auth manager the API needs.
type AuthManager interface {
	Status(ctx context.Context) auth.Status
	Probe(ctx context.Context) (auth.Status, error)
	Keepalive(ctx context.Context) error
	StartLogin(ctx context.Context) (id, url string, expiresAt time.Time, err error)
	SubmitCode(ctx context.Context, id, code string) (auth.Status, error)
	CancelLogin(ctx context.Context, id string) error
}

func (s *Server) getAuth(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		writeError(w, http.StatusNotImplemented, "unavailable", "auth manager is not enabled")
		return
	}
	writeJSON(w, http.StatusOK, s.auth.Status(r.Context()))
}

// checkAuth forces the authoritative check now, rather than waiting for the
// next keep-alive.
func (s *Server) checkAuth(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		writeError(w, http.StatusNotImplemented, "unavailable", "auth manager is not enabled")
		return
	}
	if err := s.auth.Keepalive(r.Context()); err != nil {
		// A busy executor is not an error in the credential: say so plainly.
		writeError(w, http.StatusConflict, "busy", "could not run a check now: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.auth.Status(r.Context()))
}

type loginStartResponse struct {
	AttemptID string    `json:"attempt_id"`
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

// startLogin begins the phone-friendly re-login flow (§3.2). It returns the URL
// to approve; the attempt holds the claude lock until it completes or expires.
func (s *Server) startLogin(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		writeError(w, http.StatusNotImplemented, "unavailable", "auth manager is not enabled")
		return
	}
	id, url, expiresAt, err := s.auth.StartLogin(r.Context())
	if errors.Is(err, auth.ErrLoginBusy) {
		writeError(w, http.StatusConflict, "busy", err.Error())
		return
	}
	if err != nil {
		s.serverError(w, "could not start a login", err)
		return
	}
	writeJSON(w, http.StatusCreated, loginStartResponse{AttemptID: id, URL: url, ExpiresAt: expiresAt})
}

type submitCodeRequest struct {
	Code string `json:"code"`
}

func (s *Server) submitLoginCode(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		writeError(w, http.StatusNotImplemented, "unavailable", "auth manager is not enabled")
		return
	}
	var req submitCodeRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "could not parse body: "+err.Error())
		return
	}
	status, err := s.auth.SubmitCode(r.Context(), r.PathValue("attempt"), req.Code)
	switch {
	case errors.Is(err, auth.ErrNoAttempt):
		writeError(w, http.StatusNotFound, "no_such_attempt", "no such login attempt")
		return
	case errors.Is(err, auth.ErrAttemptExpired):
		writeError(w, http.StatusGone, "attempt_expired", "that login attempt expired; start a new one")
		return
	case err != nil:
		// A wrong or stale code is the user's to fix, not a server fault.
		writeError(w, http.StatusBadRequest, "login_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) cancelLogin(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		writeError(w, http.StatusNotImplemented, "unavailable", "auth manager is not enabled")
		return
	}
	if err := s.auth.CancelLogin(r.Context(), r.PathValue("attempt")); err != nil {
		writeError(w, http.StatusNotFound, "no_such_attempt", "no such login attempt")
		return
	}
	writeJSON(w, http.StatusOK, s.auth.Status(r.Context()))
}

// listAuthEvents exposes the measured cadence, which is the whole point of
// recording it (§3.2).
func (s *Server) listAuthEvents(w http.ResponseWriter, r *http.Request) {
	events, err := s.st.AuthEvents(r.Context(), intParam(r, "limit", 100))
	if err != nil {
		s.serverError(w, "could not read auth events", err)
		return
	}
	lifetimes, err := s.st.SessionLifetimes(r.Context())
	if err != nil {
		s.serverError(w, "could not read session lifetimes", err)
		return
	}
	secs := make([]float64, 0, len(lifetimes))
	for _, d := range lifetimes {
		secs = append(secs, d.Seconds())
	}
	body := map[string]any{"events": events, "session_lifetimes_seconds": secs}
	if len(secs) == 0 {
		// Say why it is empty rather than leaving a bare [] to interpret.
		body["note"] = "no session has been observed from login to expiry yet"
	}
	writeJSON(w, http.StatusOK, body)
}
