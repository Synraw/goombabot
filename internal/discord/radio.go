package discord

import (
	"bufio"
	"encoding/binary"
	"io"
	"math"
	"os/exec"
	"runtime"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	gopus "layeh.com/gopus"
)

const (
	pcmSampleRate       = 48000            // Discord standard
	pcmChannels         = 2                // stereo
	opusFrameMillis     = 20               // 20ms frames are standard for Discord
	initialBufferFrames = 30               // ~0.6s initial buffer to start quickly
	maxBufferFrames     = 100              // ~2s max to keep latency low
	opusSendTimeout     = 1 * time.Second  // reduced from 5s - if Discord can't drain in 1s, connection is bad
	startBufferTimeout  = 3 * time.Second  // max wait for initial buffer
	healthCheckInterval = 10 * time.Second // how often to check voice connection health
)

// applyVolume applies volume scaling to PCM samples
func applyVolume(samples []int16, volume float64) {
	for i := range samples {
		// Scale sample and clamp to int16 range
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

// monitorVoiceHealth periodically checks if the voice connection is healthy
func (bot *Bot) monitorVoiceHealth(vc *discordgo.VoiceConnection, session *StreamSession) {
	ticker := time.NewTicker(healthCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-session.Context.Done():
			return
		case <-ticker.C:
			if vc == nil || !vc.Ready {
				bot.Logger.Warn("voice connection health check failed", "guild_id", session.GuildID, "vc_nil", vc == nil, "vc_ready", vc != nil && vc.Ready)
				if session.Cancel != nil {
					session.Cancel()
				}
				return
			}
		}
	}
}

// streamRadio uses ffmpeg to decode the remote stream to raw PCM and encodes
// it to Opus frames with gopus, sending directly to Discord. This keeps the
// pipeline simple and avoids custom Ogg parsing and large buffering.
func (bot *Bot) streamRadio(vc *discordgo.VoiceConnection, session *StreamSession) error {
	bot.Logger.Debug("starting radio stream", "guild_id", session.GuildID, "station", session.Station.Name)

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
		"-i", session.Station.StreamURL,
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
	cmd := exec.CommandContext(session.Context, ffmpegBin, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		bot.Logger.Error("failed to create stdout pipe", "err", err)
		return err
	}
	// Capture stderr for logging
	stderr, err := cmd.StderrPipe()
	if err != nil {
		bot.Logger.Error("failed to create stderr pipe", "err", err)
		return err
	}
	// Start ffmpeg process
	if err := cmd.Start(); err != nil {
		bot.Logger.Error("failed to start ffmpeg", "err", err)
		return err
	}

	bot.Logger.Debug("ffmpeg started", "guild_id", session.GuildID)

	// Log stderr in a separate goroutine
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			bot.Logger.Warn("ffmpeg", "guild_id", session.GuildID, "message", scanner.Text())
		}
	}()

	// Ensure ffmpeg process is cleaned up
	defer func() { _ = cmd.Wait() }()

	// Opus encoder setup
	enc, err := gopus.NewEncoder(pcmSampleRate, pcmChannels, gopus.Audio)
	if err != nil {
		bot.Logger.Error("failed to create opus encoder", "err", err)
		return err
	}

	// Set encoder parameters optimized for music
	enc.SetBitrate(128000)
	enc.SetVbr(false)

	// Calculate PCM frame parameters
	frameSamples := pcmSampleRate / (1000 / opusFrameMillis) // 960 samples per channel at 48kHz/20ms
	samplesPerFrame := frameSamples * pcmChannels            // 1920 samples total
	bytesPerFrame := samplesPerFrame * 2                     // s16le: 2 bytes per sample

	bot.Logger.Debug("frame parameters",
		"guild_id", session.GuildID,
		"frame_samples", frameSamples,
		"samples_per_frame", samplesPerFrame,
		"bytes_per_frame", bytesPerFrame)

	// Buffered channel to hold encoded Opus frames
	frames := make(chan []byte, maxBufferFrames)
	var wg sync.WaitGroup
	producerDone := make(chan struct{})
	var doneOnce sync.Once

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(frames)
		defer doneOnce.Do(func() { close(producerDone) })

		pcmBuf := make([]byte, bytesPerFrame)
		int16Buf := make([]int16, samplesPerFrame)
		framesRead := 0

		for {
			select {
			case <-session.Context.Done():
				bot.Logger.Debug("producer context cancelled", "guild_id", session.GuildID, "frames_read", framesRead)
				return
			default:
			}

			if _, err := io.ReadFull(stdout, pcmBuf); err != nil {
				if err != io.EOF {
					bot.Logger.Debug("pcm read error", "guild_id", session.GuildID, "err", err, "frames_read", framesRead)
				} else {
					bot.Logger.Debug("pcm stream ended", "guild_id", session.GuildID, "frames_read", framesRead)
				}
				return
			}

			framesRead++

			for i := range samplesPerFrame {
				int16Buf[i] = int16(binary.LittleEndian.Uint16(pcmBuf[i*2 : i*2+2]))
			}

			applyVolume(int16Buf, session.Volume)

			opus, err := enc.Encode(int16Buf, frameSamples, bytesPerFrame)
			if err != nil {
				bot.Logger.Error("opus encode failed", "guild_id", session.GuildID, "err", err)
				return
			}

			select {
			case <-session.Context.Done():
				return
			case frames <- opus:
				// Blocking when buffer is full creates natural backpressure
			}
		}
	}()

	// Start voice connection health monitor
	go bot.monitorVoiceHealth(vc, session)

	// Guard speaking toggles in case the voice connection drops
	if vc != nil && vc.Ready {
		bot.Logger.Debug("setting speaking to true", "guild_id", session.GuildID)
		vc.Speaking(true)
		defer func() {
			bot.Logger.Debug("setting speaking to false", "guild_id", session.GuildID)
			vc.Speaking(false)
		}()
	} else {
		bot.Logger.Warn("voice connection not ready or nil", "guild_id", session.GuildID, "vc_nil", vc == nil, "vc_ready", vc != nil && vc.Ready)
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
			bot.Logger.Warn("producer finished before initial buffer filled", "guild_id", session.GuildID, "frames_buffered", len(frames), "target", initialBufferFrames)
			wg.Wait()
			return nil
		case <-time.After(10 * time.Millisecond):
		}
		if time.Since(start) > startBufferTimeout {
			bot.Logger.Warn("initial buffer timeout", "guild_id", session.GuildID, "frames_buffered", len(frames), "target", initialBufferFrames, "duration", time.Since(bufferStart))
			break
		}
	}

	bot.Logger.Debug("initial buffer ready", "guild_id", session.GuildID, "frames_buffered", len(frames), "duration", time.Since(bufferStart))

	// Stable pacing scheduler using ticker
	frameDur := time.Duration(opusFrameMillis) * time.Millisecond
	ticker := time.NewTicker(frameDur)
	defer ticker.Stop()

	framesSent := 0

	for {
		select {
		case <-session.Context.Done():
			bot.Logger.Debug("stream context cancelled", "guild_id", session.GuildID, "frames_sent", framesSent)
			wg.Wait()
			return nil

		case <-ticker.C:
			// Ensure voice is ready; if not, exit to stop gracefully on disconnect
			if vc == nil || vc.OpusSend == nil || !vc.Ready {
				bot.Logger.Warn("voice connection lost during playback", "guild_id", session.GuildID, "vc_nil", vc == nil, "vc_opussend_nil", vc != nil && vc.OpusSend == nil, "vc_ready", vc != nil && vc.Ready)
				if session.Cancel != nil {
					session.Cancel() // stop producer and ffmpeg
				}
				wg.Wait()
				return nil
			}

			// Get a frame if available; if not, skip this tick to prevent jitter
			select {
			case frame, ok := <-frames:
				if !ok {
					// frames channel closed, producer finished
					bot.Logger.Debug("frames channel closed, producer finished", "guild_id", session.GuildID, "frames_sent", framesSent)
					wg.Wait()
					return nil
				}
				framesSent++

				// Double-check connection before blocking send
				if vc == nil || vc.OpusSend == nil || !vc.Ready {
					bot.Logger.Warn("voice connection became unavailable before send", "guild_id", session.GuildID, "frames_sent", framesSent)
					if session.Cancel != nil {
						session.Cancel()
					}
					wg.Wait()
					return nil
				}

				// Send with timeout and proper backpressure handling
				select {
				case vc.OpusSend <- frame:
					// Successfully sent
				case <-time.After(opusSendTimeout):
					// Add diagnostic info
					bufLen := len(vc.OpusSend)
					bufCap := cap(vc.OpusSend)
					bot.Logger.Error("opus send timeout, voice connection degraded",
						"guild_id", session.GuildID,
						"frames_sent", framesSent,
						"opus_buffer_len", bufLen,
						"opus_buffer_cap", bufCap,
						"vc_ready", vc.Ready)

					// Force disconnect the voice connection
					if vc != nil {
						bot.Logger.Debug("forcing voice disconnect due to timeout", "guild_id", session.GuildID)
						if err := vc.Disconnect(); err != nil {
							bot.Logger.Warn("error disconnecting voice after timeout", "guild_id", session.GuildID, "err", err)
						}
					}

					if session.Cancel != nil {
						session.Cancel()
					}
					wg.Wait()
					return nil
				case <-session.Context.Done():
					wg.Wait()
					return nil
				}
			default:
				continue
			}

		case <-producerDone:
			// Source finished; drain remaining frames and exit
			bot.Logger.Debug("source finished, draining remaining frames", "guild_id", session.GuildID, "frames_sent", framesSent)
			for {
				// If the voice connection is gone or not ready, exit immediately
				if vc == nil || vc.OpusSend == nil || !vc.Ready {
					bot.Logger.Debug("voice connection lost during drain", "guild_id", session.GuildID, "frames_sent", framesSent)
					if session.Cancel != nil {
						session.Cancel()
					}
					wg.Wait()
					return nil
				}
				select {
				case frame, ok := <-frames:
					if !ok {
						// frames channel closed
						bot.Logger.Debug("stream finished and drained", "guild_id", session.GuildID, "frames_sent", framesSent)
						wg.Wait()
						return nil
					}
					select {
					case vc.OpusSend <- frame:
					case <-time.After(opusSendTimeout):
						bot.Logger.Warn("opus send timeout during drain, aborting", "guild_id", session.GuildID)
						if vc != nil {
							if err := vc.Disconnect(); err != nil {
								bot.Logger.Warn("error disconnecting voice during drain timeout", "guild_id", session.GuildID, "err", err)
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
