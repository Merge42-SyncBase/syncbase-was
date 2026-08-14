package main

import (
	"context"
	"errors"
	"net"
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

func TestNewReadinessServerRejectsAddressAlreadyInUse(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	server, readinessListener, err := newReadinessServer(listener.Addr().String(), readinessFixture{})
	if err == nil {
		if readinessListener != nil {
			_ = readinessListener.Close()
		}
		t.Fatal("newReadinessServer() error = nil, want address-in-use error")
	}
	if server != nil || readinessListener != nil {
		t.Fatalf("newReadinessServer() = (%v, %v), want nil resources on error", server, readinessListener)
	}
}

type readinessFixture struct{ err error }

func (f readinessFixture) Ready(context.Context) error { return f.err }
