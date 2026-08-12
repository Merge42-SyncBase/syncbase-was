package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReadinessHandlerSeparatesLivenessAndDependencies(t *testing.T) {
	handler := newReadinessHandler(readinessFixture{err: errors.New("embedder unavailable")})
	for _, test := range []struct {
		path string
		want int
	}{
		{path: "/healthz", want: http.StatusOK},
		{path: "/readyz", want: http.StatusServiceUnavailable},
	} {
		request := httptest.NewRequest(http.MethodGet, test.path, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != test.want {
			t.Errorf("GET %s status=%d, want %d", test.path, response.Code, test.want)
		}
	}
}

func TestReadinessHandlerRequiresAllDependencies(t *testing.T) {
	handler := newReadinessHandler(readinessFixture{}, readinessFixture{})
	request := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("GET /readyz status=%d body=%s", response.Code, response.Body.String())
	}
}

type readinessFixture struct{ err error }

func (f readinessFixture) Ready(context.Context) error { return f.err }
