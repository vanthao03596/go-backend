package server

import (
	"encoding/json/v2"
	"log/slog"
	"net/http"
)

// ErrorBody is the JSON envelope returned for every non-2xx plain handler response.
// Huma handlers (phase 2) use RFC 9457 problem details instead.
type ErrorBody struct {
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.MarshalWrite(w, v); err != nil {
		slog.Default().Warn("write json response", slog.Any("err", err))
	}
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, ErrorBody{Error: code, Message: msg})
}
