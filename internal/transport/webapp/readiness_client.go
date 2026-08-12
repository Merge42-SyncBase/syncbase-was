package webapp

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// readinessClient verifies that a required internal service can process work.
type readinessClient struct {
	endpoint string
	client   *http.Client
}

func newReadinessClient(endpoint string, client *http.Client) (*readinessClient, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("invalid readiness URL")
	}
	if strings.TrimSpace(parsed.Path) == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("readiness URL must contain only a path")
	}
	if client == nil {
		return nil, fmt.Errorf("readiness HTTP client is required")
	}
	return &readinessClient{endpoint: endpoint, client: client}, nil
}

// Ready returns an error when the internal dependency is unavailable or not ready.
func (c *readinessClient) Ready(ctx context.Context) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint, nil)
	if err != nil {
		return fmt.Errorf("create readiness request: %w", err)
	}
	response, err := c.client.Do(request)
	if err != nil {
		return fmt.Errorf("request readiness: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("readiness status: %s", response.Status)
	}
	return nil
}
