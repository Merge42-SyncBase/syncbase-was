package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"
)

type readyDependency interface {
	Ready(context.Context) error
}

func newReadinessServer(address string, dependencies ...readyDependency) (*http.Server, net.Listener, error) {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, nil, fmt.Errorf("listen worker readiness: %w", err)
	}
	server := &http.Server{
		Addr:              address,
		Handler:           newReadinessHandler(dependencies...),
		ReadHeaderTimeout: 5 * time.Second,
	}
	return server, listener, nil
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
