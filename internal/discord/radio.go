package discord

import (
	"context"
	"io"
	"os/exec"
	"runtime"
)

// RadioSource represents a radio station as an audio source
type RadioSource struct {
	Station *RadioStation
	cmd     *exec.Cmd
	guildID string
	logger  interface {
		Debug(msg string, keysAndValues ...any)
		Warn(msg string, keysAndValues ...any)
		Error(msg string, keysAndValues ...any)
	}
}

// NewRadioSource creates a new radio source
func NewRadioSource(station *RadioStation, guildID string, logger interface {
	Debug(msg string, keysAndValues ...any)
	Warn(msg string, keysAndValues ...any)
	Error(msg string, keysAndValues ...any)
}) *RadioSource {
	return &RadioSource{
		Station: station,
		guildID: guildID,
		logger:  logger,
	}
}

// GetPCMReader returns an io.Reader that outputs raw PCM audio data from the radio stream
func (r *RadioSource) GetPCMReader(ctx context.Context) (io.Reader, error) {
	r.logger.Debug("starting radio stream", "guild_id", r.guildID, "station", r.Station.Name)

	r.logger.Debug("starting radio stream", "guild_id", r.guildID, "station", r.Station.Name)

	// Prepare ffmpeg command to output signed 16-bit little-endian PCM at 48kHz stereo
	ffmpegBin := "ffmpeg"
	if runtime.GOOS == "windows" {
		ffmpegBin = ".\\ffmpeg.exe"
	}

	var afiltergraph string
	if runtime.GOOS == "linux" {
		// Linux: simple resampler
		afiltergraph = "aresample=48000"
	} else {
		// Windows: async resampler smooths timing
		afiltergraph = "aresample=async=1:min_hard_comp=0.1:first_pts=0"
	}

	// Build ffmpeg arguments with enhanced reliability flags
	args := []string{
		"-loglevel", "warning",
		"-rw_timeout", "15000000",
		"-thread_queue_size", "512",
		"-i", r.Station.StreamURL,
		"-reconnect", "1",
		"-reconnect_streamed", "1",
		"-reconnect_delay_max", "5",
		"-reconnect_at_eof", "1",
		"-reconnect_on_network_error", "1",
		"-fflags", "+nobuffer+genpts",
		"-vn",
		"-af", afiltergraph,
		"-ac", "2",
		"-ar", "48000",
	}

	// Platform-specific optimizations
	if runtime.GOOS == "linux" {
		args = append(args,
			"-thread_queue_size", "256",
			"-buffer_size", "2M",
		)
	}

	// Output settings
	args = append(args,
		"-f", "s16le",
		"-acodec", "pcm_s16le",
		"pipe:1",
	)

	// Create ffmpeg command
	cmd := exec.CommandContext(ctx, ffmpegBin, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		r.logger.Error("failed to create stdout pipe", "err", err)
		return nil, err
	}

	// Capture stderr for logging
	stderr, err := cmd.StderrPipe()
	if err != nil {
		r.logger.Error("failed to create stderr pipe", "err", err)
		return nil, err
	}

	// Start ffmpeg process
	if err := cmd.Start(); err != nil {
		r.logger.Error("failed to start ffmpeg", "err", err)
		return nil, err
	}

	r.logger.Debug("ffmpeg started", "guild_id", r.guildID)

	// Store command for cleanup
	r.cmd = cmd

	// Create streaming PCM reader that logs stderr
	reader := NewStreamingPCMReader(stdout, stderr, r.guildID, r.logger)

	// Ensure cleanup when context is done
	go func() {
		<-ctx.Done()
		r.Cleanup()
	}()

	return reader, nil
}

// GetMetadata returns metadata about the radio station
func (r *RadioSource) GetMetadata() AudioMetadata {
	return AudioMetadata{
		Title:    r.Station.Name,
		Artist:   "Radio Station",
		URL:      r.Station.StreamURL,
		Duration: 0, // Live stream
		Type:     "radio",
	}
}

// Cleanup cleans up the ffmpeg process
func (r *RadioSource) Cleanup() error {
	if r.cmd != nil && r.cmd.Process != nil {
		r.logger.Debug("cleaning up radio source", "guild_id", r.guildID)
		if err := r.cmd.Process.Kill(); err != nil {
			r.logger.Warn("error killing ffmpeg process", "err", err)
		}
		_ = r.cmd.Wait()
	}
	return nil
}
