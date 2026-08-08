package webapp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Merge42-SyncBase/syncbase-was/internal/modules/knowledge"
	"github.com/google/uuid"
)

func TestMCPClientCallsRealSearchDocumentsTool(t *testing.T) {
	documentID := uuid.New()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/mcp" || request.Header.Get("Authorization") != "Bearer sb_mcp_v1_test" {
			http.Error(response, "unexpected request", http.StatusUnauthorized)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"structuredContent":{"results":[{` +
			`"rank":1,"score":0.91,"document_id":"` + documentID.String() + `",` +
			`"document_name":"보안 정책","document_version":2,"page_number":3,` +
			`"snippet":"비밀번호는 90일마다 변경합니다.",` +
			`"source_url":"http://web/sources/` + documentID.String() + `/versions/2?page=3"}]}}}`))
	}))
	t.Cleanup(server.Close)
	client, err := newMCPClient(server.URL+"/mcp", "sb_mcp_v1_test", server.Client())
	if err != nil {
		t.Fatalf("newMCPClient: %v", err)
	}
	hits, err := client.SearchDocuments(context.Background(), "비밀번호 정책", 5)
	if err != nil {
		t.Fatalf("SearchDocuments: %v", err)
	}
	if len(hits) != 1 || hits[0].DocumentID != documentID || hits[0].DocumentVersion != 2 ||
		hits[0].PageNumber != 3 || hits[0].Rank != 1 {
		t.Fatalf("hits = %+v", hits)
	}
}

func TestMCPClientMapsToolResultErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		code string
		want error
	}{
		{name: "temporary outage", code: "TEMPORARILY_UNAVAILABLE", want: knowledge.ErrTemporarilyUnavailable},
		{name: "profile mismatch", code: "PROFILE_MISMATCH", want: knowledge.ErrProfileMismatch},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				response.Header().Set("Content-Type", "application/json")
				_, _ = response.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"isError":true,"content":[{"type":"text","text":"` + test.code + `"}]}}`))
			}))
			t.Cleanup(server.Close)
			client, err := newMCPClient(server.URL+"/mcp", "sb_mcp_v1_test", server.Client())
			if err != nil {
				t.Fatalf("newMCPClient: %v", err)
			}
			_, err = client.SearchDocuments(context.Background(), "query", 5)
			if !errors.Is(err, test.want) {
				t.Fatalf("SearchDocuments error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestMCPClientReadinessUsesHealthEndpoint(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/readyz" {
			http.Error(response, "unexpected path", http.StatusNotFound)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"status":"ok"}`))
	}))
	t.Cleanup(server.Close)
	client, err := newMCPClient(server.URL+"/mcp", "sb_mcp_v1_test", server.Client())
	if err != nil {
		t.Fatalf("newMCPClient: %v", err)
	}
	if err := client.Ready(context.Background()); err != nil {
		t.Fatalf("Ready: %v", err)
	}
}
