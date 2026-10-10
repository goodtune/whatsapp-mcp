// Package httpapi exposes the plain HTTP endpoints outside MCP and OAuth:
// currently just a health check.
package httpapi

import "net/http"

func Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", handleHealthz)
}

func handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}
