package middleware

import (
	"encoding/json/v2"
	"log/slog"
	"net/http"
)

// problem is the RFC 9457 shape Huma uses for its own errors, reproduced here
// so plain middleware that runs before Huma (auth, rate limiting) answers in
// the same format as the operations it protects.
type problem struct {
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// WriteProblem writes an application/problem+json response.
func WriteProblem(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	body := problem{Title: http.StatusText(status), Status: status, Detail: detail}
	if err := json.MarshalWrite(w, body); err != nil {
		slog.Default().Warn("write problem response", slog.Any("err", err))
	}
}
