package azurecast

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	apiKeyHeader     = "X-API-Key"
	defaultUserAgent = "goombabot-azurecast-client"
	maxErrorBody     = 4 << 10 // 4KiB
)

// Option configures a Client instance.
type Option func(*Client)

// Client provides a minimal AzuraCast API surface needed by goombabot.
type Client struct {
	baseURL    *url.URL
	httpClient *http.Client
	apiKey     string
	userAgent  string
}

// NewClient constructs a Client for the given AzuraCast API base URL.
// The base URL should include the "/api" prefix exposed by AzuraCast.
func NewClient(baseURL string, opts ...Option) (*Client, error) {
	if strings.TrimSpace(baseURL) == "" {
		return nil, errors.New("azurecast: baseURL must not be empty")
	}

	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("azurecast: parse baseURL: %w", err)
	}

	// Ensure we never end up with double slashes when appending paths.
	parsed.Path = strings.TrimRight(parsed.Path, "/")

	client := &Client{
		baseURL: parsed,
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
		},
		userAgent: defaultUserAgent,
	}

	for _, opt := range opts {
		opt(client)
	}

	if client.httpClient == nil {
		client.httpClient = &http.Client{Timeout: 15 * time.Second}
	}

	return client, nil
}

// WithHTTPClient replaces the default http.Client.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) {
		c.httpClient = hc
	}
}

// WithAPIKey configures the client to send the provided AzuraCast API key.
func WithAPIKey(key string) Option {
	return func(c *Client) {
		c.apiKey = key
	}
}

// WithUserAgent overrides the default User-Agent header.
func WithUserAgent(agent string) Option {
	return func(c *Client) {
		c.userAgent = agent
	}
}

// APIError describes a non-2xx response returned by AzuraCast.
type APIError struct {
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("azurecast: unexpected status %d: %s", e.StatusCode, e.Body)
}

func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	rel, err := url.Parse(path)
	if err != nil {
		return nil, fmt.Errorf("azurecast: parse path: %w", err)
	}

	resolved := c.baseURL.ResolveReference(rel)
	req, err := http.NewRequestWithContext(ctx, method, resolved.String(), body)
	if err != nil {
		return nil, fmt.Errorf("azurecast: new request: %w", err)
	}

	req.Header.Set("Accept", "application/json")
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}
	if c.apiKey != "" {
		req.Header.Set(apiKeyHeader, c.apiKey)
	}

	return req, nil
}

func (c *Client) do(req *http.Request, v any) error {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("azurecast: do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return &APIError{StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(snippet))}
	}

	if v == nil {
		io.Copy(io.Discard, resp.Body)
		return nil
	}

	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("azurecast: decode response: %w", err)
	}

	return nil
}

// GetStations retrieves the list of stations from AzuraCast.
func (c *Client) GetStations(ctx context.Context) ([]Stations, error) {

	req, err := c.newRequest(ctx, http.MethodGet, "/api/stations", nil)
	if err != nil {
		return nil, err
	}
	var payload []Stations
	if err := c.do(req, &payload); err != nil {
		return nil, err
	}
	return payload, nil
}

// GetNowPlaying retrieves the "now playing" details for all stations.
func (c *Client) GetNowPlaying(ctx context.Context) ([]NowPlaying, error) {

	req, err := c.newRequest(ctx, http.MethodGet, "/api/nowplaying", nil)
	if err != nil {
		return nil, err
	}

	var payload []NowPlaying
	if err := c.do(req, &payload); err != nil {
		return nil, err
	}

	return payload, nil
}

// GetStationNowPlaying retrieves the "now playing" details for the given station.
// stationID should match the shortcode or numeric ID accepted by AzuraCast.
func (c *Client) GetStationNowPlaying(ctx context.Context, stationID string) (*NowPlaying, error) {
	if strings.TrimSpace(stationID) == "" {
		return nil, errors.New("azurecast: stationID must not be empty")
	}

	req, err := c.newRequest(ctx, http.MethodGet, "/api/nowplaying/"+url.PathEscape(stationID), nil)
	if err != nil {
		return nil, err
	}

	var payload NowPlaying
	if err := c.do(req, &payload); err != nil {
		return nil, err
	}

	return &payload, nil
}
