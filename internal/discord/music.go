package discord

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// MusicSource represents a music source from yt-dlp (YouTube, SoundCloud, etc.)
type MusicSource struct {
	URL      string
	Metadata AudioMetadata
	cmd      *exec.Cmd
	userID   string
	guildID  string
	logger   interface {
		Debug(msg string, keysAndValues ...any)
		Warn(msg string, keysAndValues ...any)
		Error(msg string, keysAndValues ...any)
	}
}

// YtDlpMetadata represents the metadata returned by yt-dlp
type YtDlpMetadata struct {
	Title       string  `json:"title"`
	Uploader    string  `json:"uploader"`
	Duration    float64 `json:"duration"`
	URL         string  `json:"url"`
	Webpage_URL string  `json:"webpage_url"`
	Extractor   string  `json:"extractor"`
}

// NewMusicSource creates a new music source from a URL using yt-dlp
func NewMusicSource(url, userID string, guildID string, logger interface {
	Debug(msg string, keysAndValues ...any)
	Warn(msg string, keysAndValues ...any)
	Error(msg string, keysAndValues ...any)
}) (*MusicSource, error) {
	// First, extract metadata using yt-dlp
	metadata, err := extractMetadata(url, logger)
	if err != nil {
		return nil, fmt.Errorf("failed to extract metadata: %w", err)
	}

	source := &MusicSource{
		URL:     url,
		guildID: guildID,
		userID:  userID,
		logger:  logger,
		Metadata: AudioMetadata{
			Title:    metadata.Title,
			Artist:   metadata.Uploader,
			URL:      metadata.Webpage_URL,
			Duration: time.Duration(metadata.Duration) * time.Second,
			Type:     normalizeExtractor(metadata.Extractor),
		},
	}

	return source, nil
}

// extractMetadata uses yt-dlp to extract metadata from a URL
func extractMetadata(url string, logger interface {
	Debug(msg string, keysAndValues ...any)
	Warn(msg string, keysAndValues ...any)
	Error(msg string, keysAndValues ...any)
}) (*YtDlpMetadata, error) {
	ytDlpBin := "yt-dlp"
	if runtime.GOOS == "windows" {
		ytDlpBin = ".\\yt-dlp.exe"
	}

	args := []string{
		"--dump-json",
		"--no-playlist",
		"--skip-download",
		url,
	}

	cmd := exec.Command(ytDlpBin, args...)
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("yt-dlp failed to extract metadata: %w", err)
	}

	var metadata YtDlpMetadata
	if err := json.Unmarshal(output, &metadata); err != nil {
		return nil, fmt.Errorf("failed to parse yt-dlp metadata: %w", err)
	}

	logger.Debug("extracted metadata", "title", metadata.Title, "uploader", metadata.Uploader, "duration", metadata.Duration)

	return &metadata, nil
}

// normalizeExtractor converts yt-dlp extractor names to friendly names
func normalizeExtractor(extractor string) string {
	extractor = strings.ToLower(extractor)
	if strings.Contains(extractor, "youtube") {
		return "youtube"
	}
	if strings.Contains(extractor, "soundcloud") {
		return "soundcloud"
	}
	if strings.Contains(extractor, "spotify") {
		return "spotify"
	}
	if strings.Contains(extractor, "bandcamp") {
		return "bandcamp"
	}
	if strings.Contains(extractor, "twitch") {
		return "twitch"
	}
	return "music"
}

// GetPCMReader returns an io.Reader that outputs raw PCM audio data from yt-dlp
func (m *MusicSource) GetPCMReader(ctx context.Context) (io.Reader, error) {
	ytDlpBin := "yt-dlp"
	if runtime.GOOS == "windows" {
		ytDlpBin = ".\\yt-dlp.exe"
	}

	ffmpegBin := "ffmpeg"
	if runtime.GOOS == "windows" {
		ffmpegBin = ".\\ffmpeg.exe"
	}

	// yt-dlp arguments to get best audio and output URL
	ytDlpArgs := []string{
		"--format", "bestaudio/best",
		"--no-playlist",
		"--output", "-",
		"--quiet",
		m.URL,
	}

	// Build ffmpeg filter graph
	var afiltergraph string
	if runtime.GOOS == "linux" {
		afiltergraph = "aresample=48000"
	} else {
		afiltergraph = "aresample=async=1:min_hard_comp=0.1:first_pts=0"
	}

	// ffmpeg arguments to convert to PCM
	ffmpegArgs := []string{
		"-loglevel", "warning",
		"-i", "pipe:0",
		"-vn",
		"-af", afiltergraph,
		"-ac", "2",
		"-ar", "48000",
		"-f", "s16le",
		"-acodec", "pcm_s16le",
		"pipe:1",
	}

	// Create yt-dlp command
	ytDlpCmd := exec.CommandContext(ctx, ytDlpBin, ytDlpArgs...)

	// Create ffmpeg command
	ffmpegCmd := exec.CommandContext(ctx, ffmpegBin, ffmpegArgs...)

	// Pipe yt-dlp output to ffmpeg input
	ytDlpStdout, err := ytDlpCmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create yt-dlp stdout pipe: %w", err)
	}
	ffmpegCmd.Stdin = ytDlpStdout

	// Get ffmpeg output pipes
	ffmpegStdout, err := ffmpegCmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create ffmpeg stdout pipe: %w", err)
	}

	ffmpegStderr, err := ffmpegCmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create ffmpeg stderr pipe: %w", err)
	}

	// Get yt-dlp stderr for logging
	ytDlpStderr, err := ytDlpCmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create yt-dlp stderr pipe: %w", err)
	}

	// Start yt-dlp
	if err := ytDlpCmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start yt-dlp: %w", err)
	}

	// Start ffmpeg
	if err := ffmpegCmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start ffmpeg: %w", err)
	}

	m.logger.Debug("started yt-dlp and ffmpeg pipeline", "guild_id", m.guildID, "url", m.URL)

	// Store commands for cleanup
	m.cmd = ffmpegCmd

	// Log yt-dlp stderr
	go func() {
		scanner := NewStreamingPCMReader(nil, ytDlpStderr, m.guildID, m.logger)
		scanner.logStderr()
	}()

	// Create streaming PCM reader that logs ffmpeg stderr
	reader := NewStreamingPCMReader(ffmpegStdout, ffmpegStderr, m.guildID, m.logger)

	// Ensure cleanup when context is done
	go func() {
		<-ctx.Done()
		m.Cleanup()
	}()

	return reader, nil
}

// GetMetadata returns metadata about the music source
func (m *MusicSource) GetMetadata() AudioMetadata {
	return m.Metadata
}

// Cleanup cleans up the yt-dlp and ffmpeg processes
func (m *MusicSource) Cleanup() error {
	if m.cmd != nil && m.cmd.Process != nil {
		m.logger.Debug("cleaning up music source", "guild_id", m.guildID)
		if err := m.cmd.Process.Kill(); err != nil {
			m.logger.Warn("error killing ffmpeg process", "err", err)
		}
		_ = m.cmd.Wait()
	}
	return nil
}

// MusicQueue represents a queue of music sources to be played
type MusicQueue struct {
	items   []*MusicSource
	current int
}

// NewMusicQueue creates a new music queue
func NewMusicQueue() *MusicQueue {
	return &MusicQueue{
		items:   make([]*MusicSource, 0),
		current: -1,
	}
}

// Add adds a music source to the queue
func (q *MusicQueue) Add(source *MusicSource) {
	q.items = append(q.items, source)
}

// Next returns the next music source in the queue and removes the previous one
func (q *MusicQueue) Next() *MusicSource {
	// If there's a current song, remove it before moving to the next
	if q.current >= 0 && q.current < len(q.items) {
		q.items = append(q.items[:q.current], q.items[q.current+1:]...)
	} else if q.current == -1 {
		// Starting for the first time - move to position 0
		q.current = 0
	}

	// Return the next song if available
	if q.current < len(q.items) {
		return q.items[q.current]
	}
	return nil
}

// Current returns the current music source
func (q *MusicQueue) Current() *MusicSource {
	if q.current >= 0 && q.current < len(q.items) {
		return q.items[q.current]
	}
	return nil
}

// SetCurrent sets the current track index
func (q *MusicQueue) SetCurrent(index int) bool {
	if index >= 0 && index < len(q.items) {
		q.current = index
		return true
	}
	return false
}

// Clear clears the queue
func (q *MusicQueue) Clear() {
	q.items = make([]*MusicSource, 0)
	q.current = -1
}

// Size returns the size of the queue
func (q *MusicQueue) Size() int {
	return len(q.items)
}

// IsEmpty returns true if the queue is empty
func (q *MusicQueue) IsEmpty() bool {
	return len(q.items) == 0
}

// Remove removes an item from the queue by index
func (q *MusicQueue) Remove(index int) bool {
	if index < 0 || index >= len(q.items) {
		return false
	}
	q.items = append(q.items[:index], q.items[index+1:]...)
	if q.current >= index {
		q.current--
	}
	return true
}

// List returns a list of all items in the queue
func (q *MusicQueue) List() []*MusicSource {
	return q.items
}
