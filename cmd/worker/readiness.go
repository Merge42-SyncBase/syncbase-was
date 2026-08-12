package main

import (
	"context"
	"net/http"
	"time"
)

type readyDependency interface {
	Ready(context.Context) error
}

// newReadinessHandler exposes worker liveness separately from its dependencies.
// The API calls /readyz before declaring the document platform ready, so a
// process that cannot read originals, parse, embed, or persist work is never
// treated as healthy.
func newReadinessHandler(dependencies ...readyDependency) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /readyz", func(response http.ResponseWriter, request *http.Request) {
		ctx, cancel := context.WithTimeout(request.Context(), 2*time.Second)
		defer cancel()
		for _, dependency := range dependencies {
			if dependency == nil || dependency.Ready(ctx) != nil {
				http.Error(response, "worker dependency is not ready", http.StatusServiceUnavailable)
				return
			}
		}
		response.WriteHeader(http.StatusOK)
	})
	return mux
}
