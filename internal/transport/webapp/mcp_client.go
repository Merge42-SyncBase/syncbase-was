package webapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/Merge42-SyncBase/syncbase-was/internal/modules/knowledge"
)

type mcpClient struct {
	endpoint      string
	readyEndpoint string
	token         string
	httpClient    *http.Client
}

func newMCPClient(endpoint, token string, httpClient *http.Client) (*mcpClient, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" ||
		strings.TrimSpace(token) == "" || httpClient == nil {
		return nil, knowledge.ErrInvalidArgument
	}
	readyURL := *parsed
	readyURL.Path = "/readyz"
	readyURL.RawPath = ""
	readyURL.RawQuery = ""
	readyURL.Fragment = ""
	return &mcpClient{
		endpoint: endpoint, readyEndpoint: readyURL.String(),
		token: strings.TrimSpace(token), httpClient: httpClient,
	}, nil
}

func (c *mcpClient) Ready(ctx context.Context) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.readyEndpoint, nil)
	if err != nil {
		return fmt.Errorf("build MCP readiness request: %w", err)
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("check MCP readiness: %w", knowledge.ErrTemporarilyUnavailable)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4*1024))
	if response.StatusCode != http.StatusOK {
		return knowledge.ErrTemporarilyUnavailable
	}
	return nil
}

func (c *mcpClient) SearchDocuments(
	ctx context.Context,
	query string,
	limit int,
) ([]knowledge.SearchHit, error) {
	query = strings.TrimSpace(query)
	if query == "" || len([]rune(query)) > 2000 || limit < 1 || limit > 20 {
		return nil, knowledge.ErrInvalidArgument
	}
	payload := map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name":      "search_documents",
			"arguments": map[string]any{"query": query, "limit": limit},
		},
	}
	content, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode MCP search request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(content))
	if err != nil {
		return nil, fmt.Errorf("build MCP search request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	response, err := c.httpClient.Do(request)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, fmt.Errorf("call MCP search: %w", knowledge.ErrTemporarilyUnavailable)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 2*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("read MCP search response: %w", knowledge.ErrTemporarilyUnavailable)
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return nil, knowledge.ErrUnauthenticated
	}
	if response.StatusCode != http.StatusOK {
		return nil, knowledge.ErrTemporarilyUnavailable
	}
	var result struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			StructuredContent struct {
				Results []knowledge.SearchHit `json:"results"`
			} `json:"structuredContent"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
			Data    struct {
				Code string `json:"code"`
			} `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decode MCP search response: %w", knowledge.ErrTemporarilyUnavailable)
	}
	if result.Error != nil {
		if err := mcpSearchError(result.Error.Data.Code); err != nil {
			return nil, err
		}
		return nil, errors.New("MCP search failed")
	}
	if result.Result.IsError {
		for _, content := range result.Result.Content {
			if content.Type != "text" {
				continue
			}
			if err := mcpSearchError(content.Text); err != nil {
				return nil, err
			}
		}
		return nil, errors.New("MCP search failed")
	}
	return result.Result.StructuredContent.Results, nil
}

func mcpSearchError(code string) error {
	switch strings.TrimSpace(code) {
	case "PROFILE_MISMATCH":
		return knowledge.ErrProfileMismatch
	case "TEMPORARILY_UNAVAILABLE":
		return knowledge.ErrTemporarilyUnavailable
	default:
		return nil
	}
}
