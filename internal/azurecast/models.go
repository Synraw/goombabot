package azurecast

type NowPlaying struct {
	Station struct {
		ID              int    `json:"id"`
		Name            string `json:"name"`
		Shortcode       string `json:"shortcode"`
		Description     string `json:"description"`
		Frontend        string `json:"frontend"`
		Backend         string `json:"backend"`
		Timezone        string `json:"timezone"`
		ListenURL       string `json:"listen_url"`
		URL             string `json:"url"`
		PublicPlayerURL string `json:"public_player_url"`
		PlaylistPlsURL  string `json:"playlist_pls_url"`
		PlaylistM3UURL  string `json:"playlist_m3u_url"`
		IsPublic        bool   `json:"is_public"`
		RequestsEnabled bool   `json:"requests_enabled"`
		Mounts          []struct {
			ID        int    `json:"id"`
			Name      string `json:"name"`
			URL       string `json:"url"`
			Bitrate   int    `json:"bitrate"`
			Format    string `json:"format"`
			Listeners struct {
				Total   int `json:"total"`
				Unique  int `json:"unique"`
				Current int `json:"current"`
			} `json:"listeners"`
			Path      string `json:"path"`
			IsDefault bool   `json:"is_default"`
		} `json:"mounts"`
		Remotes []struct {
			ID        int    `json:"id"`
			Name      string `json:"name"`
			URL       string `json:"url"`
			Bitrate   int    `json:"bitrate"`
			Format    string `json:"format"`
			Listeners struct {
				Total   int `json:"total"`
				Unique  int `json:"unique"`
				Current int `json:"current"`
			} `json:"listeners"`
		} `json:"remotes"`
		HlsEnabled   bool   `json:"hls_enabled"`
		HlsIsDefault bool   `json:"hls_is_default"`
		HlsURL       string `json:"hls_url"`
		HlsListeners int    `json:"hls_listeners"`
	} `json:"station"`
	Listeners struct {
		Total   int `json:"total"`
		Unique  int `json:"unique"`
		Current int `json:"current"`
	} `json:"listeners"`
	Live struct {
		IsLive         bool   `json:"is_live"`
		StreamerName   string `json:"streamer_name"`
		BroadcastStart int    `json:"broadcast_start"`
		Art            string `json:"art"`
	} `json:"live"`
	NowPlaying struct {
		ShID      int    `json:"sh_id"`
		PlayedAt  int    `json:"played_at"`
		Duration  int    `json:"duration"`
		Playlist  string `json:"playlist"`
		Streamer  string `json:"streamer"`
		IsRequest bool   `json:"is_request"`
		Song      struct {
			Text         string   `json:"text"`
			Artist       string   `json:"artist"`
			Title        string   `json:"title"`
			Album        string   `json:"album"`
			Genre        string   `json:"genre"`
			Isrc         string   `json:"isrc"`
			Lyrics       string   `json:"lyrics"`
			ID           string   `json:"id"`
			Art          string   `json:"art"`
			CustomFields []string `json:"custom_fields"`
		} `json:"song"`
		Elapsed   int `json:"elapsed"`
		Remaining int `json:"remaining"`
	} `json:"now_playing"`
	PlayingNext struct {
		CuedAt    int    `json:"cued_at"`
		PlayedAt  int    `json:"played_at"`
		Duration  int    `json:"duration"`
		Playlist  string `json:"playlist"`
		IsRequest bool   `json:"is_request"`
		Song      struct {
			Text         string   `json:"text"`
			Artist       string   `json:"artist"`
			Title        string   `json:"title"`
			Album        string   `json:"album"`
			Genre        string   `json:"genre"`
			Isrc         string   `json:"isrc"`
			Lyrics       string   `json:"lyrics"`
			ID           string   `json:"id"`
			Art          string   `json:"art"`
			CustomFields []string `json:"custom_fields"`
		} `json:"song"`
	} `json:"playing_next"`
	SongHistory []struct {
		ShID      int    `json:"sh_id"`
		PlayedAt  int    `json:"played_at"`
		Duration  int    `json:"duration"`
		Playlist  string `json:"playlist"`
		Streamer  string `json:"streamer"`
		IsRequest bool   `json:"is_request"`
		Song      struct {
			Text         string   `json:"text"`
			Artist       string   `json:"artist"`
			Title        string   `json:"title"`
			Album        string   `json:"album"`
			Genre        string   `json:"genre"`
			Isrc         string   `json:"isrc"`
			Lyrics       string   `json:"lyrics"`
			ID           string   `json:"id"`
			Art          string   `json:"art"`
			CustomFields []string `json:"custom_fields"`
		} `json:"song"`
	} `json:"song_history"`
	IsOnline bool   `json:"is_online"`
	Cache    string `json:"cache"`
}

type Stations struct {
	ID              int    `json:"id"`
	Name            string `json:"name"`
	Shortcode       string `json:"shortcode"`
	Description     string `json:"description"`
	Frontend        string `json:"frontend"`
	Backend         string `json:"backend"`
	Timezone        string `json:"timezone"`
	ListenURL       string `json:"listen_url"`
	URL             string `json:"url"`
	PublicPlayerURL string `json:"public_player_url"`
	PlaylistPlsURL  string `json:"playlist_pls_url"`
	PlaylistM3UURL  string `json:"playlist_m3u_url"`
	IsPublic        bool   `json:"is_public"`
	RequestsEnabled bool   `json:"requests_enabled"`
	Mounts          []struct {
		ID        int    `json:"id"`
		Name      string `json:"name"`
		URL       string `json:"url"`
		Bitrate   int    `json:"bitrate"`
		Format    string `json:"format"`
		Listeners struct {
			Total   int `json:"total"`
			Unique  int `json:"unique"`
			Current int `json:"current"`
		} `json:"listeners"`
		Path      string `json:"path"`
		IsDefault bool   `json:"is_default"`
	} `json:"mounts"`
	Remotes      []interface{} `json:"remotes"`
	HlsEnabled   bool          `json:"hls_enabled"`
	HlsIsDefault bool          `json:"hls_is_default"`
	HlsURL       interface{}   `json:"hls_url"`
	HlsListeners int           `json:"hls_listeners"`
}
