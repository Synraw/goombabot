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
	if !strings.HasSuffix(parsed.Path, "/") {
		parsed.Path += "/"
	}

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
		return nil, errors.New("azurecast: context must not be nil")
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

// validateStationID is a helper to validate station IDs.
func validateStationID(stationID string) error {
	if strings.TrimSpace(stationID) == "" {
		return errors.New("azurecast: stationID must not be empty")
	}
	return nil
}

// request is a helper function to send a request and handle the response.
func (c *Client) request(ctx context.Context, method, path string, body io.Reader, v any) error {
	req, err := c.newRequest(ctx, method, path, body)
	if err != nil {
		return err
	}
	return c.do(req, v)
}

// do sends the HTTP request and decodes the response.
func (c *Client) do(req *http.Request, v any) error {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("azurecast: do request: %w", err)
	}
	defer resp.Body.Close()

	// Check for non-2xx status codes
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return &APIError{
			StatusCode: resp.StatusCode,
			Body:       string(body),
		}
	}

	// If no response body expected (v is nil), we're done
	if v == nil {
		return nil
	}

	// For 204 No Content, don't try to decode
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}

	// Decode the response body into v
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		return fmt.Errorf("azurecast: decode response: %w", err)
	}

	return nil
}

// GetStations retrieves the list of stations from AzuraCast.
func (c *Client) GetStations(ctx context.Context) ([]Stations, error) {
	var payload []Stations
	err := c.request(ctx, http.MethodGet, "stations", nil, &payload)
	return payload, err
}

// GetNowPlaying retrieves the "now playing" details for all stations.
func (c *Client) GetNowPlaying(ctx context.Context) ([]NowPlaying, error) {
	var payload []NowPlaying
	err := c.request(ctx, http.MethodGet, "nowplaying", nil, &payload)
	return payload, err
}

// GetStationNowPlaying retrieves the "now playing" details for the given station.
// stationID should match the shortcode or numeric ID accepted by AzuraCast.
func (c *Client) GetStationNowPlaying(ctx context.Context, stationID string) (*NowPlaying, error) {
	if err := validateStationID(stationID); err != nil {
		return nil, err
	}
	var payload NowPlaying
	err := c.request(ctx, http.MethodGet, "nowplaying/"+url.PathEscape(stationID), nil, &payload)
	return &payload, err
}

// SkipCurrentSong requests AzuraCast to skip the currently playing song
func (c *Client) SkipCurrentSong(ctx context.Context, stationID string) error {
	if err := validateStationID(stationID); err != nil {
		return err
	}
	return c.request(ctx, http.MethodPost, "station/"+url.PathEscape(stationID)+"/backend/skip", nil, nil)
}

// GetStationRequestableSongs retrieves the list of requestable songs for the given station.
func (c *Client) GetStationRequestableSongs(ctx context.Context, stationID string) ([]StationSongRequest, error) {
	if err := validateStationID(stationID); err != nil {
		return nil, err
	}
	var payload []StationSongRequest
	err := c.request(ctx, http.MethodGet, "station/"+url.PathEscape(stationID)+"/requests", nil, &payload)
	return payload, err
}

// RequestStationSong requests a song to be played on the given station.
func (c *Client) RequestStationSong(ctx context.Context, stationID string, requestID StationSongRequest) (response StandardResponse, err error) {
	if err := validateStationID(stationID); err != nil {
		return StandardResponse{}, err
	}
	err = c.request(ctx, http.MethodPost, "station/"+url.PathEscape(stationID)+"/request/"+url.PathEscape(requestID.RequestID), nil, &response)
	return
}
