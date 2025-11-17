package azurecast

import "encoding/json"

// NowPlaying models the subset of the AzuraCast now-playing response that the bot consumes.
type NowPlaying struct {
	Station     Station            `json:"station"`
	Listeners   ListenerCounts     `json:"listeners"`
	Live        LiveStatus         `json:"live"`
	NowPlaying  NowPlayingEntry    `json:"now_playing"`
	PlayingNext *NowPlayingEntry   `json:"playing_next,omitempty"`
	SongHistory []SongHistoryEntry `json:"song_history,omitempty"`
	IsOnline    bool               `json:"is_online"`
	Cache       json.RawMessage    `json:"cache,omitempty"`
}

// Station summarises metadata for the station the payload refers to.
type Station struct {
	ID        int     `json:"id"`
	Name      string  `json:"name"`
	Shortcode string  `json:"shortcode"`
	URL       *string `json:"url,omitempty"`
}

// ListenerCounts contains listener totals.
type ListenerCounts struct {
	Total   int `json:"total"`
	Unique  int `json:"unique"`
	Current int `json:"current"`
}

// LiveStatus indicates whether a live DJ is currently connected.
type LiveStatus struct {
	IsLive         bool   `json:"is_live"`
	StreamerName   string `json:"streamer_name"`
	Streamer       string `json:"streamer"`
	BroadcastStart *int64 `json:"broadcast_start,omitempty"`
}

// NowPlayingEntry provides detailed track information.
type NowPlayingEntry struct {
	Song      Song   `json:"song"`
	Duration  int    `json:"duration"`
	Elapsed   int    `json:"elapsed"`
	Remaining int    `json:"remaining"`
	Playlist  string `json:"playlist"`
	Streamer  string `json:"streamer"`
}

// Song captures basic metadata about a media item.
type Song struct {
	ID     string  `json:"id"`
	Text   string  `json:"text"`
	Artist *string `json:"artist,omitempty"`
	Title  *string `json:"title,omitempty"`
	Album  *string `json:"album,omitempty"`
}

// SongHistoryEntry describes a recently played track.
type SongHistoryEntry struct {
	Song     Song   `json:"song"`
	PlayedAt int64  `json:"played_at"`
	Duration int    `json:"duration"`
	Playlist string `json:"playlist"`
}
