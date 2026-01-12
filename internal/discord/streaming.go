package discord

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	gopus "layeh.com/gopus"
)

const (
	bitrateKbps         = 128              // 128 kbps
	pcmSampleRate       = 48000            // Discord standard
	pcmChannels         = 2                // stereo
	opusFrameMillis     = 20               // 20ms frames are standard for Discord
	initialBufferFrames = 30               // ~0.6s initial buffer to start quickly
	maxBufferFrames     = 100              // ~2s max to keep latency low
	opusSendTimeout     = 1 * time.Second  // per-send timeout
	opusRetryTimeout    = 5 * time.Second  // max time to retry before force disconnect
	startBufferTimeout  = 3 * time.Second  // max wait for initial buffer
	healthCheckInterval = 10 * time.Second // how often to check voice connection health
	readyCheckThreshold = 3 * time.Second  // threshold for waiting for voice connection to be ready
)

type AudioRepeatType int

const (
	AudioRepeatNone AudioRepeatType = iota
	AudioRepeatOne
	AudioRepeatAll
)

func (art AudioRepeatType) String() string {
	return [...]string{"none", "one", "all"}[art]
}

// AudioSource represents any source of audio that can be streamed to Discord.
// Implementations include radio streams, yt-dlp music sources, local files, etc.
type AudioSource interface {
	// GetPCMReader returns an io.Reader that outputs raw PCM audio data.
	// The PCM format should be: signed 16-bit little-endian, 48kHz, stereo (2 channels).
	// The context can be used to cancel/cleanup the audio source.
	GetPCMReader(ctx context.Context) (io.Reader, error)

	// GetMetadata returns metadata about the audio source (title, artist, URL, etc.)
	GetMetadata() AudioMetadata

	// Cleanup is called when the stream is finished to release any resources.
	Cleanup() error
}

// AudioMetadata contains information about an audio source.
type AudioMetadata struct {
	Title    string        // Title of the audio (song name, stream name, etc.)
	Artist   string        // Artist name (if applicable)
	URL      string        // Source URL
	Duration time.Duration // Duration (0 for live streams)
	Type     string        // Type of source ("radio", "youtube", "soundcloud", etc.)
}

// StreamSession represents an active audio streaming session in a guild.
type StreamSession struct {
	Context    context.Context    // context for managing the stream lifecycle
	Cancel     context.CancelFunc // function to cancel the stream
	UserID     string             // ID of the user who initiated the stream
	GuildID    string             // ID of the guild where the stream is playing
	Volume     float64            // volume level (0.0 to 1.0)
	RepeatMode AudioRepeatType    // repeat mode for the stream (only valid for music streams, not radios)
	Source     AudioSource        // the audio source being streamed
}

// VoiceStreamer handles streaming audio from any AudioSource to Discord voice.
type VoiceStreamer struct {
	bot    *Bot
	logger interface {
		Debug(msg string, keysAndValues ...any)
		Warn(msg string, keysAndValues ...any)
		Error(msg string, keysAndValues ...any)
	}
}

// NewVoiceStreamer creates a new VoiceStreamer.
func NewVoiceStreamer(bot *Bot) *VoiceStreamer {
	return &VoiceStreamer{
		bot:    bot,
		logger: bot.Logger,
	}
}

// Stream starts streaming audio from the given source to the Discord voice connection.
func (vs *VoiceStreamer) Stream(vc *discordgo.VoiceConnection, session *StreamSession) error {
	vs.logger.Debug("starting audio stream", "guild_id", session.GuildID, "type", session.Source.GetMetadata().Type)

	// Get PCM reader from the audio source
	pcmReader, err := session.Source.GetPCMReader(session.Context)
	if err != nil {
		vs.logger.Error("failed to get PCM reader from source", "err", err)
		return err
	}

	// Ensure cleanup on exit
	defer func() {
		if err := session.Source.Cleanup(); err != nil {
			vs.logger.Warn("error cleaning up audio source", "err", err)
		}
	}()

	// Opus encoder setup
	enc, err := gopus.NewEncoder(pcmSampleRate, pcmChannels, gopus.Audio)
	if err != nil {
		vs.logger.Error("failed to create opus encoder", "err", err)
		return err
	}

	// Set encoder parameters optimized for music
	enc.SetBitrate(bitrateKbps * 1000) // in bps
	enc.SetVbr(false)                  // constant bitrate

	// Calculate PCM frame parameters
	frameSamples := pcmSampleRate / (1000 / opusFrameMillis) // 960 samples per channel at 48kHz/20ms
	samplesPerFrame := frameSamples * pcmChannels            // 1920 samples total
	bytesPerFrame := samplesPerFrame * 2                     // s16le: 2 bytes per sample

	vs.logger.Debug("frame parameters",
		"guild_id", session.GuildID,
		"frame_samples", frameSamples,
		"samples_per_frame", samplesPerFrame,
		"bytes_per_frame", bytesPerFrame)

	// Buffered channel to hold encoded Opus frames
	frames := make(chan []byte, maxBufferFrames)
	var wg sync.WaitGroup
	producerDone := make(chan struct{})
	var doneOnce sync.Once

	lastReadyCheckTime := time.Now()

	// Producer goroutine: reads PCM data, encodes to Opus, and sends to frames channel
	wg.Go(func() {
		defer close(frames)
		defer doneOnce.Do(func() { close(producerDone) })

		pcmBuf := make([]byte, bytesPerFrame)
		int16Buf := make([]int16, samplesPerFrame)
		framesRead := 0

		for {
			select {
			case <-session.Context.Done():
				vs.logger.Debug("producer context cancelled", "guild_id", session.GuildID, "frames_read", framesRead)
				return
			default:
			}

			// Read one frame of PCM data
			if _, err := io.ReadFull(pcmReader, pcmBuf); err != nil {
				if err != io.EOF {
					vs.logger.Debug("pcm read error", "guild_id", session.GuildID, "err", err, "frames_read", framesRead)
				} else {
					vs.logger.Debug("pcm stream ended", "guild_id", session.GuildID, "frames_read", framesRead)
				}
				return
			}

			framesRead++

			// Convert bytes to int16 samples
			for i := range samplesPerFrame {
				int16Buf[i] = int16(binary.LittleEndian.Uint16(pcmBuf[i*2 : i*2+2]))
			}

			// Apply volume
			applyVolume(int16Buf, session.Volume)

			// Encode to Opus
			opus, err := enc.Encode(int16Buf, frameSamples, bytesPerFrame)
			if err != nil {
				vs.logger.Error("opus encode failed", "guild_id", session.GuildID, "err", err)
				return
			}

			// Send to frames channel (blocking creates backpressure)
			select {
			case <-session.Context.Done():
				return
			case frames <- opus:
			}
		}
	})

	// Start voice connection health monitor
	go vs.monitorVoiceHealth(vc, session)

	// Guard speaking toggles
	if vc != nil && vc.Ready {
		vs.logger.Debug("setting speaking to true", "guild_id", session.GuildID)
		vc.Speaking(true)
		defer func() {
			vs.logger.Debug("setting speaking to false", "guild_id", session.GuildID)
			vc.Speaking(false)
		}()
	} else {
		vs.logger.Warn("voice connection not ready or nil", "guild_id", session.GuildID, "vc_nil", vc == nil, "vc_ready", vc != nil && vc.Ready)
		if session.Cancel != nil {
			session.Cancel()
		}
		return errors.New("voice connection not ready or nil at start of stream")
	}

	// Wait for initial buffer or timeout
	start := time.Now()
	bufferStart := time.Now()
	for len(frames) < initialBufferFrames {
		select {
		case <-session.Context.Done():
			wg.Wait()
			return nil
		case <-producerDone:
			vs.logger.Warn("producer finished before initial buffer filled", "guild_id", session.GuildID, "frames_buffered", len(frames), "target", initialBufferFrames)
			wg.Wait()
			return nil
		case <-time.After(10 * time.Millisecond):
		}
		if time.Since(start) > startBufferTimeout {
			vs.logger.Warn("initial buffer timeout", "guild_id", session.GuildID, "frames_buffered", len(frames), "target", initialBufferFrames, "duration", time.Since(bufferStart))
			break
		}
	}

	vs.logger.Debug("initial buffer ready", "guild_id", session.GuildID, "frames_buffered", len(frames), "duration", time.Since(bufferStart))

	// Stable pacing scheduler using ticker
	frameDur := time.Duration(opusFrameMillis) * time.Millisecond
	ticker := time.NewTicker(frameDur)
	defer ticker.Stop()

	framesSent := 0
	consecutiveTimeouts := 0

	for {
		select {
		case <-session.Context.Done():
			vs.logger.Debug("stream context cancelled", "guild_id", session.GuildID, "frames_sent", framesSent)
			wg.Wait()
			return nil

		case <-ticker.C:
			// Check if connection has been not-ready for longer than threshold
			if vc == nil || vc.OpusSend == nil || !vc.Ready {
				if time.Since(lastReadyCheckTime) > readyCheckThreshold {
					vs.logger.Warn("voice connection lost during playback", "guild_id", session.GuildID, "vc_nil", vc == nil, "vc_opussend_nil", vc != nil && vc.OpusSend == nil, "vc_ready", vc != nil && vc.Ready)
					if session.Cancel != nil {
						session.Cancel()
					}
					wg.Wait()
					return nil
				}
				continue
			}

			lastReadyCheckTime = time.Now()

			// Get a frame if available
			select {
			case frame, ok := <-frames:
				if !ok {
					vs.logger.Debug("frames channel closed, producer finished", "guild_id", session.GuildID, "frames_sent", framesSent)
					wg.Wait()
					return nil
				}
				framesSent++
				consecutiveTimeouts = 0

				// Double-check connection before send
				if vc == nil || vc.OpusSend == nil || !vc.Ready {
					vs.logger.Warn("voice connection became unavailable before send", "guild_id", session.GuildID, "frames_sent", framesSent)
					if session.Cancel != nil {
						session.Cancel()
					}
					wg.Wait()
					return nil
				}

				// Send with timeout
				select {
				case vc.OpusSend <- frame:
				case <-time.After(opusSendTimeout):
					consecutiveTimeouts++
					bufLen := len(vc.OpusSend)
					bufCap := cap(vc.OpusSend)
					vs.logger.Warn("opus send timeout, waiting for Discord to drain",
						"guild_id", session.GuildID,
						"frames_sent", framesSent,
						"consecutive_timeouts", consecutiveTimeouts,
						"opus_buffer_len", bufLen,
						"opus_buffer_cap", bufCap,
						"vc_ready", vc.Ready)

					if consecutiveTimeouts > int(opusRetryTimeout/opusSendTimeout) {
						vs.logger.Error("opus send timeouts exceeded retry threshold, force disconnecting",
							"guild_id", session.GuildID,
							"frames_sent", framesSent,
							"consecutive_timeouts", consecutiveTimeouts)

						if vc != nil {
							vs.logger.Debug("forcing voice disconnect due to persistent timeouts", "guild_id", session.GuildID)
							if err := vc.Disconnect(); err != nil {
								vs.logger.Warn("error disconnecting voice after persistent timeouts", "guild_id", session.GuildID, "err", err)
							}
						}

						if session.Cancel != nil {
							session.Cancel()
						}
						wg.Wait()
						return nil
					}

					// Re-queue frame or drop if buffer full
					select {
					case frames <- frame:
						vs.logger.Debug("re-queued frame after timeout", "guild_id", session.GuildID, "consecutive_timeouts", consecutiveTimeouts)
					default:
						vs.logger.Debug("dropped frame due to full buffer after timeout", "guild_id", session.GuildID)
					}

				case <-session.Context.Done():
					wg.Wait()
					return nil
				}
			default:
				continue
			}

		case <-producerDone:
			// Drain remaining frames
			vs.logger.Debug("source finished, draining remaining frames", "guild_id", session.GuildID, "frames_sent", framesSent)
			for {
				if vc == nil || vc.OpusSend == nil || !vc.Ready {
					vs.logger.Debug("voice connection lost during drain", "guild_id", session.GuildID, "frames_sent", framesSent)
					if session.Cancel != nil {
						session.Cancel()
					}
					wg.Wait()
					return nil
				}
				select {
				case frame, ok := <-frames:
					if !ok {
						vs.logger.Debug("stream finished and drained", "guild_id", session.GuildID, "frames_sent", framesSent)
						wg.Wait()
						return nil
					}
					select {
					case vc.OpusSend <- frame:
					case <-time.After(opusSendTimeout):
						vs.logger.Warn("opus send timeout during drain, aborting", "guild_id", session.GuildID)
						if vc != nil {
							if err := vc.Disconnect(); err != nil {
								vs.logger.Warn("error disconnecting voice during drain timeout", "guild_id", session.GuildID, "err", err)
							}
						}
						wg.Wait()
						return nil
					}
				default:
					wg.Wait()
					return nil
				}
			}
		}
	}
}

// monitorVoiceHealth periodically checks if the voice connection is healthy.
func (vs *VoiceStreamer) monitorVoiceHealth(vc *discordgo.VoiceConnection, session *StreamSession) {
	ticker := time.NewTicker(healthCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-session.Context.Done():
			return
		case <-ticker.C:
			if vc == nil || !vc.Ready {
				vs.logger.Warn("voice connection health check failed", "guild_id", session.GuildID, "vc_nil", vc == nil, "vc_ready", vc != nil && vc.Ready)
				if session.Cancel != nil {
					session.Cancel()
				}
				return
			}
		}
	}
}

// applyVolume applies volume scaling to PCM samples.
func applyVolume(samples []int16, volume float64) {
	for i := range samples {
		scaled := float64(samples[i]) * volume
		if scaled > math.MaxInt16 {
			samples[i] = math.MaxInt16
		} else if scaled < math.MinInt16 {
			samples[i] = math.MinInt16
		} else {
			samples[i] = int16(scaled)
		}
	}
}

// StreamingPCMReader wraps an exec.Cmd's stdout and stderr for PCM streaming with logging.
type StreamingPCMReader struct {
	stdout io.Reader
	stderr io.Reader
	logger interface {
		Warn(msg string, keysAndValues ...any)
	}
	guildID string
}

// NewStreamingPCMReader creates a PCM reader that logs stderr messages.
func NewStreamingPCMReader(stdout, stderr io.Reader, guildID string, logger interface {
	Warn(msg string, keysAndValues ...any)
}) *StreamingPCMReader {
	reader := &StreamingPCMReader{
		stdout:  stdout,
		stderr:  stderr,
		logger:  logger,
		guildID: guildID,
	}

	// Start logging stderr in background
	go reader.logStderr()

	return reader
}

// Read implements io.Reader for the PCM data.
func (r *StreamingPCMReader) Read(p []byte) (n int, err error) {
	return r.stdout.Read(p)
}

// logStderr logs stderr output from the process.
func (r *StreamingPCMReader) logStderr() {
	if r.stderr == nil {
		return
	}
	scanner := bufio.NewScanner(r.stderr)
	for scanner.Scan() {
		r.logger.Warn("ffmpeg", "guild_id", r.guildID, "message", scanner.Text())
	}
}
