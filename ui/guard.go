package main

import (
	"log/slog"
	"net/http"
)

// placeholder: replaced by the real guard.
func newGuard(cfg Config, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler { return next }
}
