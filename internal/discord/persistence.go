package discord

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// GuildSessionState represents persisted session data for a guild
type GuildSessionState struct {
	GuildID   string  `json:"guild_id"`
	StationID int     `json:"station_id"`
	Volume    float64 `json:"volume"`
}

// SessionStore handles loading and saving guild session states
type SessionStore struct {
	mu       sync.RWMutex
	filePath string
	states   map[string]*GuildSessionState
}

// NewSessionStore creates a new session store with the given file path
func NewSessionStore(dataDir string) (*SessionStore, error) {
	// Create data directory if it doesn't exist
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, err
	}

	filePath := filepath.Join(dataDir, "sessions.json")
	store := &SessionStore{
		filePath: filePath,
		states:   make(map[string]*GuildSessionState),
	}

	// Load existing sessions
	if err := store.Load(); err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	return store, nil
}

// Load reads session states from disk
func (ss *SessionStore) Load() error {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	data, err := os.ReadFile(ss.filePath)
	if err != nil {
		return err
	}

	var states []GuildSessionState
	if err := json.Unmarshal(data, &states); err != nil {
		return err
	}

	ss.states = make(map[string]*GuildSessionState)
	for i := range states {
		ss.states[states[i].GuildID] = &states[i]
	}

	return nil
}

// Save writes session states to disk
func (ss *SessionStore) Save() error {
	ss.mu.RLock()
	states := make([]GuildSessionState, 0, len(ss.states))
	for _, state := range ss.states {
		states = append(states, *state)
	}
	ss.mu.RUnlock()

	data, err := json.MarshalIndent(states, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(ss.filePath, data, 0644)
}

// Get retrieves a guild's session state
func (ss *SessionStore) Get(guildID string) *GuildSessionState {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	return ss.states[guildID]
}

// Set saves a guild's session state and persists it
func (ss *SessionStore) Set(guildID string, stationID int, volume float64) error {
	ss.mu.Lock()
	ss.states[guildID] = &GuildSessionState{
		GuildID:   guildID,
		StationID: stationID,
		Volume:    volume,
	}
	ss.mu.Unlock()

	return ss.Save()
}

// Delete removes a guild's session state
func (ss *SessionStore) Delete(guildID string) error {
	ss.mu.Lock()
	delete(ss.states, guildID)
	ss.mu.Unlock()

	return ss.Save()
}
