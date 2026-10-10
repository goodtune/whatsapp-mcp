// Package httpapi exposes the plain REST endpoints a client needs before it
// has an MCP session: registering an account and logging in to obtain the
// JWT used to authenticate the MCP endpoint.
package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/goodtune/whatsapp-mcp/internal/auth"
)

type Handler struct {
	auth *auth.Service
}

func New(authService *auth.Service) *Handler {
	return &Handler{auth: authService}
}

func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/register", h.handleRegister)
	mux.HandleFunc("POST /api/login", h.handleLogin)
	mux.HandleFunc("GET /healthz", h.handleHealthz)
}

type credentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type tokenResponse struct {
	Token    string `json:"token"`
	Username string `json:"username"`
}

func (h *Handler) handleRegister(w http.ResponseWriter, r *http.Request) {
	var creds credentials
	if err := json.NewDecoder(r.Body).Decode(&creds); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	user, err := h.auth.Register(r.Context(), creds.Username, creds.Password)
	if err != nil {
		if errors.Is(err, auth.ErrUsernameTaken) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	token, err := h.auth.IssueToken(user)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to issue token")
		return
	}

	writeJSON(w, http.StatusCreated, tokenResponse{Token: token, Username: user.Username})
}

func (h *Handler) handleLogin(w http.ResponseWriter, r *http.Request) {
	var creds credentials
	if err := json.NewDecoder(r.Body).Decode(&creds); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	user, err := h.auth.Authenticate(r.Context(), creds.Username, creds.Password)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid username or password")
		return
	}

	token, err := h.auth.IssueToken(user)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to issue token")
		return
	}

	writeJSON(w, http.StatusOK, tokenResponse{Token: token, Username: user.Username})
}

func (h *Handler) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
