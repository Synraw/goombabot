package azurecast

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetStationNowPlaying(t *testing.T) {
	t.Parallel()

	var (
		receivedPath   string
		receivedAPIKey string
		receivedUA     string
	)

	sample := `{"station":{"id":1,"name":"Test Station","shortcode":"test","url":"https://example.com"},"listeners":{"total":10,"unique":5,"current":7},"live":{"is_live":false,"streamer_name":"","streamer":"","broadcast_start":null,"art":null},"now_playing":{"song":{"id":"abc","text":"Artist - Title","artist":"Artist","title":"Title","album":null},"duration":180,"elapsed":30,"remaining":150,"playlist":"General Rotation","streamer":""},"playing_next":null,"song_history":[{"song":{"id":"prev","text":"Prev","artist":"Artist","title":"Prev Title","album":null},"played_at":1700000000,"duration":200,"playlist":"General Rotation"}],"is_online":true,"cache":{}}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		receivedAPIKey = r.Header.Get(apiKeyHeader)
		receivedUA = r.Header.Get("User-Agent")

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(sample))
	}))
	t.Cleanup(srv.Close)

	client, err := NewClient(srv.URL, WithAPIKey("secret"), WithUserAgent("test-agent"))
	if err != nil {
		t.Fatalf("NewClient returned error: %v", err)
	}

	resp, err := client.GetStationNowPlaying(context.Background(), "station123")
	if err != nil {
		t.Fatalf("GetStationNowPlaying returned error: %v", err)
	}

	if receivedPath != "/nowplaying/station123" {
		t.Fatalf("unexpected request path: %s", receivedPath)
	}

	if receivedAPIKey != "secret" {
		t.Fatalf("expected API key header to be propagated, got %q", receivedAPIKey)
	}

	if receivedUA != "test-agent" {
		t.Fatalf("expected user agent header to be propagated, got %q", receivedUA)
	}

	if resp.Station.Name != "Test Station" {
		t.Fatalf("unexpected station name: %s", resp.Station.Name)
	}

	if resp.Listeners.Current != 7 {
		t.Fatalf("unexpected listeners current value: %d", resp.Listeners.Current)
	}

	if resp.NowPlaying.Song.Text != "Artist - Title" {
		t.Fatalf("unexpected now playing text: %s", resp.NowPlaying.Song.Text)
	}

	if len(resp.SongHistory) != 1 {
		t.Fatalf("expected song history length 1, got %d", len(resp.SongHistory))
	}
}

func TestGetStationNowPlayingRequiresStationID(t *testing.T) {
	t.Parallel()

	client, err := NewClient("https://example.com/api")
	if err != nil {
		t.Fatalf("unexpected error constructing client: %v", err)
	}

	if _, err := client.GetStationNowPlaying(context.Background(), " "); err == nil {
		t.Fatal("expected error for empty stationID, got nil")
	}
}
