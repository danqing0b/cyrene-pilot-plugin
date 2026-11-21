// SPDX-License-Identifier: Apache-2.0
package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
)

func (c *coordinator) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /admin/releases/{id}", c.operator(c.adminRelease))
	mux.HandleFunc("POST /admin/releases/{id}/approve", c.operator(c.adminApprove))
	mux.HandleFunc("POST /admin/releases/{id}/refresh", c.operator(c.adminApprove))
	mux.HandleFunc("POST /admin/releases/{id}/feedback", c.operator(c.adminFeedback))
	mux.HandleFunc("POST /v1/chat/completions", c.chat)
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func reportError(w http.ResponseWriter, err error) {
	status, message := http.StatusServiceUnavailable, "release operation unavailable"
	switch {
	case errors.Is(err, errConflict):
		status, message = http.StatusConflict, err.Error()
	case errors.Is(err, errNotFound):
		status, message = http.StatusNotFound, err.Error()
	case terminal(err):
		status, message = http.StatusUnprocessableEntity, err.Error()
	}
	writeJSON(w, status, map[string]string{"error": message})
}

func authorized(r *http.Request, token string) bool {
	return token != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) == 1
}

func (c *coordinator) operator(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r, c.product.cfg.adminToken) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "operator authorization is required"})
			return
		}
		next(w, r)
	}
}

func (c *coordinator) adminRelease(w http.ResponseWriter, r *http.Request) {
	s, err := c.store.view()
	if err != nil {
		reportError(w, err)
		return
	}
	entry := s.Releases[r.PathValue("id")]
	if entry == nil {
		reportError(w, errNotFound)
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

func (c *coordinator) adminApprove(w http.ResponseWriter, r *http.Request) {
	var args approval
	if strictJSON(http.MaxBytesReader(w, r.Body, 4096), &args) != nil {
		reportError(w, permanentError("invalid approval"))
		return
	}
	if err := c.approve(r.Context(), r.PathValue("id"), args, r.Pattern == "POST /admin/releases/{id}/refresh"); err != nil {
		reportError(w, err)
		return
	}
	c.adminRelease(w, r)
}

func (c *coordinator) adminFeedback(w http.ResponseWriter, r *http.Request) {
	var args feedbackRequest
	if strictJSON(http.MaxBytesReader(w, r.Body, 32_768), &args) != nil {
		reportError(w, permanentError("invalid feedback"))
		return
	}
	if err := c.acceptFeedback(r.PathValue("id"), args); err != nil {
		reportError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}

func (c *coordinator) chat(w http.ResponseWriter, r *http.Request) {
	if !authorized(r, c.product.cfg.pilotToken) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "pilot authorization is required"})
		return
	}
	var request map[string]json.RawMessage
	if strictJSON(http.MaxBytesReader(w, r.Body, maxBody), &request) != nil {
		reportError(w, permanentError("invalid chat request"))
		return
	}
	var alias string
	if json.Unmarshal(request["model"], &alias) != nil || alias != pilotAlias {
		reportError(w, permanentError("the pilot accepts only the release-pilot model alias"))
		return
	}
	if raw, ok := request["stream"]; ok && string(raw) != "false" {
		reportError(w, permanentError("the pilot accepts non-streaming chat only"))
		return
	}
	state, err := c.store.view()
	if err != nil {
		reportError(w, err)
		return
	}
	request["model"], _ = json.Marshal(state.Active.Alias)
	reply, requestID, err := c.product.raw(r.Context(), c.product.cfg.Inference, http.MethodPost, "/v1/chat/completions", "", request)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "Exchange inference failed"})
		return
	}
	var response struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(reply, &response) != nil || response.Model != state.Active.Alias {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "Exchange returned an unexpected model"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Request-Id", requestID)
	w.Header().Set("X-Cyrene-Pilot-Route", state.Active.ID)
	_, _ = w.Write(reply)
}
