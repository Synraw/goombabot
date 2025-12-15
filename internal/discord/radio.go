package discord

import (
	"bufio"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"time"

	"github.com/bwmarrin/discordgo"
)

const (
	// Streaming constants
	packetBufferSize    = 2000
	initialBufferSize   = 150
	maxBufferSize       = 400
	tickInterval        = 20 * time.Millisecond
	opusSendTimeout     = 500 * time.Millisecond
	maxInvalidOggPages  = 10
	maxConsecutiveEmpty = 200 // 4 seconds of empty ticks (increased from 50 for long streams)

	// Stream health monitoring
	healthCheckInterval = 5 * time.Second
	minPacketsPerSecond = 5 // Minimum healthy packet rate

	// Ogg constants
	oggMagicNumber = "OggS"
	oggHeaderSize  = 27
	oggMagicOffset = 4

	// Opus constants
	opusHeadSignature = "OpusHead"
	opusTagsSignature = "OpusTags"
	minOpusPacketSize = 1
	opusSignatureSize = 8

	// FFmpeg constants
	ffmpegBinary        = "ffmpeg"
	ffmpegWindowsBinary = ".\\ffmpeg.exe"
	sampleRate          = 48000
	channels            = 2
	bitrate             = "128k"
	frameDuration       = "20"
	bufferSize          = "256k"
	maxMuxingQueueSize  = 1024

	// HTTP streaming constants
	httpReadBufferSize  = 256 * 1024
	httpWriteBufferSize = 256 * 1024
	httpIdleTimeout     = 90 * time.Second
	httpDialTimeout     = 10 * time.Second
	httpKeepAlive       = 30 * time.Second
	oggReaderBufferSize = 128 * 1024

	// Content types
	contentTypeOgg      = "application/ogg"
	contentTypeAudioOgg = "audio/ogg"

	// Voice connection constants
	voiceReconnectAttempts = 3
	voiceReconnectDelay    = 2 * time.Second
	voiceNotReadyTimeout   = 10 * time.Second // Consider connection lost if not ready for this long
)

var (
	ErrInvalidOggHeader    = errors.New("invalid ogg page header")
	ErrTooManyInvalidPages = errors.New("too many invalid ogg pages")
	ErrSegmentOverflow     = errors.New("segment overflow in ogg page")
	ErrBufferStarvation    = errors.New("buffer starvation detected")
	ErrStreamEnded         = errors.New("stream ended unexpectedly")
	ErrInvalidContentType  = errors.New("stream is not in Ogg/Opus format")
	ErrBufferNotFilled     = errors.New("stream exited before buffer filled")
)

// isOpusMetadataPacket checks if a packet is OpusHead or OpusTags
func isOpusMetadataPacket(packet []byte) bool {
	if len(packet) < opusSignatureSize {
		return false
	}
	sig := string(packet[:opusSignatureSize])
	return sig == opusHeadSignature || sig == opusTagsSignature
}

// isValidOpusPacket validates an Opus packet
func isValidOpusPacket(packet []byte) bool {
	return len(packet) >= minOpusPacketSize && !isOpusMetadataPacket(packet)
}

// parseOggOpusStream reads Ogg pages from a reader and sends Opus packets to a channel.
func (bot *Bot) parseOggOpusStream(ctx context.Context, reader *bufio.Reader, packets chan<- []byte, errChan chan<- error) {
	defer close(packets)

	var pending []byte
	invalidPacketCount := 0

	for {
		select {
		case <-ctx.Done():
			bot.Logger.Debug("parseOggOpusStream context cancelled")
			return
		default:
		}

		// Read Ogg page header
		header := make([]byte, oggHeaderSize)
		if _, err := io.ReadFull(reader, header); err != nil {
			// Don't log as error if context was cancelled
			if ctx.Err() != nil {
				bot.Logger.Debug("ogg read cancelled", "reason", ctx.Err())
				return
			}
			if err != io.EOF && err != io.ErrUnexpectedEOF {
				bot.Logger.Error("ogg read header error", "err", err, "headerData", hex.EncodeToString(header))
				select {
				case errChan <- fmt.Errorf("ogg header read failed: %w", err):
				case <-ctx.Done():
				}
			} else {
				bot.Logger.Debug("ogg stream ended", "err", err)
			}
			return
		}

		// Validate Ogg magic number
		if string(header[0:oggMagicOffset]) != oggMagicNumber {
			invalidPacketCount++
			if invalidPacketCount > maxInvalidOggPages {
				bot.Logger.Error("too many invalid Ogg pages, aborting")
				select {
				case errChan <- ErrTooManyInvalidPages:
				case <-ctx.Done():
				}
				return
			}
			bot.Logger.Warn("invalid ogg page, skipping", "invalidCount", invalidPacketCount)
			continue
		}
		invalidPacketCount = 0

		// Read segment table
		segCount := int(header[26])
		lacingVals := make([]byte, segCount)
		if _, err := io.ReadFull(reader, lacingVals); err != nil {
			if ctx.Err() != nil {
				bot.Logger.Debug("lacing read cancelled", "reason", ctx.Err())
				return
			}
			bot.Logger.Error("ogg read lacing error", "err", err, "segmentCount", segCount)
			select {
			case errChan <- fmt.Errorf("lacing read failed: %w", err):
			case <-ctx.Done():
			}
			return
		}

		// Calculate page size
		pageSize := 0
		for _, v := range lacingVals {
			pageSize += int(v)
		}

		// Read page data
		pageData := make([]byte, pageSize)
		if _, err := io.ReadFull(reader, pageData); err != nil {
			if ctx.Err() != nil {
				bot.Logger.Debug("page data read cancelled", "reason", ctx.Err())
				return
			}
			bot.Logger.Error("ogg read page data error", "err", err, "expectedSize", pageSize)
			select {
			case errChan <- fmt.Errorf("page data read failed: %w", err):
			case <-ctx.Done():
			}
			return
		}

		// Process segments
		if err := bot.processOggSegments(ctx, lacingVals, pageData, &pending, packets); err != nil {
			if err == ErrSegmentOverflow {
				bot.Logger.Warn("segment overflow, skipping rest of page")
				continue
			}
			if ctx.Err() != nil {
				bot.Logger.Debug("segment processing cancelled")
				return
			}
			select {
			case errChan <- err:
			case <-ctx.Done():
			}
			return
		}
	}
}

// processOggSegments extracts Opus packets from Ogg page segments
func (bot *Bot) processOggSegments(ctx context.Context, lacingVals, pageData []byte, pending *[]byte, packets chan<- []byte) error {
	offset := 0
	for _, lv := range lacingVals {
		size := int(lv)
		if size == 0 {
			continue
		}

		if offset+size > len(pageData) {
			return ErrSegmentOverflow
		}

		seg := pageData[offset : offset+size]
		offset += size
		*pending = append(*pending, seg...)

		// Packet is complete when segment size < 255
		if size < 255 {
			packet := *pending
			*pending = nil

			// Skip metadata packets and validate
			if !isValidOpusPacket(packet) {
				continue
			}

			// Send packet (make copy to avoid data races)
			frame := make([]byte, len(packet))
			copy(frame, packet)

			if err := bot.sendPacketWithTimeout(ctx, packets, frame, 100*time.Millisecond); err != nil {
				return err
			}
		}
	}
	return nil
}

// sendPacketWithTimeout sends a packet with timeout and congestion handling
func (bot *Bot) sendPacketWithTimeout(ctx context.Context, packets chan<- []byte, frame []byte, timeout time.Duration) error {
	select {
	case packets <- frame:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(timeout):
		bot.Logger.Warn("packet channel congestion", "channelLen", len(packets), "channelCap", cap(packets))
		// Try one more time after warning
		select {
		case packets <- frame:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// sendOpusPackets handles buffering and sending Opus packets to Discord.
func (bot *Bot) sendOpusPackets(vc *discordgo.VoiceConnection, session *StreamSession, packets <-chan []byte, errChan <-chan error, guildID, channelID string) error {
	ringBuffer := make([][]byte, 0, maxBufferSize)

	// Fill initial buffer
	if err := bot.fillInitialBuffer(session.Context, &ringBuffer, packets, errChan); err != nil {
		bot.Logger.Debug("initial buffer fill failed", "err", err)
		return err
	}

	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	healthTicker := time.NewTicker(healthCheckInterval)
	defer healthTicker.Stop()

	var stats streamStats
	consecutiveEmpty := 0
	lastOverflowLog := time.Time{}
	stats.lastHealthCheckTime = time.Now()
	stats.lastHealthCheckPackets = 0

	for {
		select {
		case <-session.Context.Done():
			bot.Logger.Debug("sendOpusPackets context cancelled")
			bot.logStreamStats(&stats)
			return nil

		case err := <-errChan:
			if session.Context.Err() != nil {
				bot.Logger.Debug("error received after context cancelled", "err", err)
				bot.logStreamStats(&stats)
				return nil
			}
			bot.Logger.Error("stream error received", "err", err)
			bot.logStreamStats(&stats)
			return err

		case pkt, ok := <-packets:
			if !ok {
				// Check if context was cancelled
				if session.Context.Err() != nil {
					bot.Logger.Debug("packet channel closed due to context cancellation")
					bot.logStreamStats(&stats)
					return nil
				}
				bot.Logger.Warn("packet channel closed unexpectedly")
				bot.logStreamStats(&stats)
				return ErrStreamEnded
			}

			ringBuffer = append(ringBuffer, pkt)
			stats.totalPacketsReceived++

			// Handle buffer overflow
			if len(ringBuffer) > maxBufferSize {
				dropCount := len(ringBuffer) - maxBufferSize
				stats.packetsDropped += dropCount
				ringBuffer = ringBuffer[dropCount:]

				// Only log every second to avoid spam
				if time.Since(lastOverflowLog) > time.Second {
					bot.Logger.Warn("ring buffer overflow",
						"dropping", dropCount,
						"totalDropped", stats.packetsDropped,
						"bufferLen", len(ringBuffer),
						"opusChanLen", len(vc.OpusSend),
						"opusChanCap", cap(vc.OpusSend))
					lastOverflowLog = time.Now()
				}
			}

		case <-healthTicker.C:
			// Check stream health every 5 seconds
			elapsed := time.Since(stats.lastHealthCheckTime).Seconds()
			packetDelta := stats.totalPacketsReceived - stats.lastHealthCheckPackets
			packetsPerSecond := float64(packetDelta) / elapsed

			if packetsPerSecond < float64(minPacketsPerSecond) && len(ringBuffer) < initialBufferSize {
				stats.lowPacketRateCount++
				bot.Logger.Warn("low packet rate detected",
					"packetsPerSecond", packetsPerSecond,
					"bufferLen", len(ringBuffer),
					"lowRateCount", stats.lowPacketRateCount)

				// If low packet rate persists, attempt HTTP reconnection for direct streams
				if stats.lowPacketRateCount >= 3 {
					bot.Logger.Warn("persistent low packet rate, stream may have stalled")
					// Don't fail immediately; let the parser timeout handle it
					stats.lowPacketRateCount = 0
				}
			} else {
				stats.lowPacketRateCount = 0
			}

			stats.lastHealthCheckTime = time.Now()
			stats.lastHealthCheckPackets = stats.totalPacketsReceived

		case <-ticker.C:
			if len(ringBuffer) == 0 {
				consecutiveEmpty++
				if consecutiveEmpty >= maxConsecutiveEmpty {
					// Check if context was cancelled first
					if session.Context.Err() != nil {
						bot.Logger.Debug("buffer empty due to context cancellation")
						bot.logStreamStats(&stats)
						return nil
					}
					bot.Logger.Error("buffer starved", "count", consecutiveEmpty, "bufferLen", len(ringBuffer))
					bot.logStreamStats(&stats)
					return ErrBufferStarvation
				}
				continue
			}
			consecutiveEmpty = 0

			sendBatch := 1
			if len(ringBuffer) > initialBufferSize {
				sendBatch = 3 // catch up when backlog is big
			}

			for n := 0; n < sendBatch && len(ringBuffer) > 0; n++ {
				if err := bot.sendNextPacket(&vc, session.Context, &ringBuffer, &stats, session, guildID, channelID); err != nil {
					if err == context.Canceled || err == context.DeadlineExceeded {
						bot.Logger.Debug("sendNextPacket cancelled", "err", err)
						bot.logStreamStats(&stats)
						return nil
					}
					return err
				}
			}
		}
	}
}

// streamStats tracks streaming metrics
type streamStats struct {
	totalPacketsReceived   int
	totalPacketsSent       int
	packetsDropped         int
	sendTimeouts           int
	notReadyCount          int
	lastHealthCheckPackets int
	lastHealthCheckTime    time.Time
	lowPacketRateCount     int
}

// fillInitialBuffer fills the buffer to initialBufferSize before starting playback
func (bot *Bot) fillInitialBuffer(ctx context.Context, ringBuffer *[][]byte, packets <-chan []byte, errChan <-chan error) error {
	fillCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	for len(*ringBuffer) < initialBufferSize {
		select {
		case <-fillCtx.Done():
			return fmt.Errorf("initial buffer fill timeout: got %d/%d packets", len(*ringBuffer), initialBufferSize)
		case pkt, ok := <-packets:
			if !ok {
				return ErrBufferNotFilled
			}
			*ringBuffer = append(*ringBuffer, pkt)
		case err := <-errChan:
			return err
		}
	}
	return nil
}

// sendNextPacket sends the next packet from the buffer to Discord
func (bot *Bot) sendNextPacket(vc **discordgo.VoiceConnection, ctx context.Context, ringBuffer *[][]byte, stats *streamStats, session *StreamSession, guildID, channelID string) error {
	if *vc == nil {
		return errors.New("voice connection nil")
	}

	// If the voice connection is not ready, attempt reconnection after timeout
	if !(*vc).Ready {
		stats.notReadyCount++

		// Log periodically
		if stats.notReadyCount%50 == 1 { // Log at 1, 51, 101, etc (every ~1 second)
			bot.Logger.Warn("voice not ready; waiting to resume",
				"waitTicks", stats.notReadyCount,
				"bufferLen", len(*ringBuffer),
				"totalSent", stats.totalPacketsSent)
		}

		// If not ready for too long, attempt reconnection
		notReadyDuration := time.Duration(stats.notReadyCount) * tickInterval
		if notReadyDuration >= voiceNotReadyTimeout {
			bot.Logger.Warn("voice connection not ready for too long, attempting reconnection",
				"duration", notReadyDuration,
				"waitTicks", stats.notReadyCount)

			newVC, err := bot.reconnectVoice(session, guildID, channelID)
			if err != nil {
				// Non-fatal: keep buffering and retry later.
				bot.Logger.Error("voice reconnection failed; will retry", "err", err)
				// Reset the counter so we wait another voiceNotReadyTimeout before next attempt.
				stats.notReadyCount = 0
				// Small pause to avoid tight retry loops during outages.
				time.Sleep(500 * time.Millisecond)
				return nil
			}

			// Successfully reconnected
			*vc = newVC
			stats.notReadyCount = 0

			// Wait a moment for the connection to stabilize before resuming
			time.Sleep(500 * time.Millisecond)

			// Start speaking on the new connection
			(*vc).Speaking(true)
			bot.Logger.Info("voice connection restored, resuming playback")
		}

		return nil
	}

	// Ready again; reset counter
	stats.notReadyCount = 0

	packet := (*ringBuffer)[0]
	*ringBuffer = (*ringBuffer)[1:]

	if len(packet) == 0 {
		return nil
	}

	// Safety check - ensure OpusSend channel exists and is not nil
	if (*vc).OpusSend == nil {
		bot.Logger.Warn("opus send channel is nil, voice connection not fully initialized")
		stats.notReadyCount++ // Treat this as not ready
		return nil
	}

	// Try sending the packet with timeout
	select {
	case (*vc).OpusSend <- packet:
		stats.totalPacketsSent++
		stats.sendTimeouts = 0
		return nil

	case <-time.After(opusSendTimeout):
		stats.sendTimeouts++
		if stats.sendTimeouts == 1 || stats.sendTimeouts%10 == 0 {
			bot.Logger.Warn("opus send timeout",
				"bufferLen", len(*ringBuffer),
				"timeouts", stats.sendTimeouts,
				"totalSent", stats.totalPacketsSent,
				"opusSendChanLen", len((*vc).OpusSend),
				"ready", (*vc).Ready)
		}
		if stats.sendTimeouts >= 100 {
			bot.Logger.Error("opus channel consistently blocked, likely disconnected",
				"timeouts", stats.sendTimeouts,
				"opusChanLen", len((*vc).OpusSend))
			return errors.New("opus send channel blocked")
		}
		return nil

	case <-ctx.Done():
		return ctx.Err()
	}
}

// logStreamStats logs final streaming statistics
func (bot *Bot) logStreamStats(stats *streamStats) {
	bot.Logger.Info("stream ended",
		"packetsReceived", stats.totalPacketsReceived,
		"packetsSent", stats.totalPacketsSent,
		"packetsDropped", stats.packetsDropped,
		"finalTimeouts", stats.sendTimeouts)
}

// reconnectVoice attempts to reconnect to a voice channel
func (bot *Bot) reconnectVoice(session *StreamSession, guildID, channelID string) (*discordgo.VoiceConnection, error) {
	bot.Logger.Info("attempting voice reconnection", "guild_id", guildID, "channel_id", channelID)

	// If we have a prior VC, leave cleanly to avoid stuck voice state
	if vc, exists := bot.Session.VoiceConnections[guildID]; exists && vc != nil {
		vc.Speaking(false)
		// Best-effort: leave the current channel and close sockets
		_ = vc.Disconnect()
		vc.Close()
		time.Sleep(500 * time.Millisecond)
		// Remove stale entry
		delete(bot.Session.VoiceConnections, guildID)
		bot.Logger.Debug("cleaned up prior voice connection")
	}

	for attempt := 1; attempt <= voiceReconnectAttempts; attempt++ {
		select {
		case <-session.Context.Done():
			return nil, session.Context.Err()
		default:
		}

		bot.Logger.Info("voice reconnection attempt", "attempt", attempt, "maxAttempts", voiceReconnectAttempts)

		vc, err := bot.Session.ChannelVoiceJoin(guildID, channelID, false, true)
		if err != nil {
			bot.Logger.Warn("voice reconnection failed", "attempt", attempt, "err", err)
			if attempt < voiceReconnectAttempts {
				time.Sleep(voiceReconnectDelay * time.Duration(attempt)) // Exponential backoff
				continue
			}
			return nil, fmt.Errorf("failed to reconnect after %d attempts: %w", voiceReconnectAttempts, err)
		}

		// Wait for connection to be ready
		for range 30 { // Wait up to 3 seconds
			if vc.Ready {
				bot.Logger.Info("voice reconnection successful", "attempt", attempt)
				return vc, nil
			}
			time.Sleep(100 * time.Millisecond)
		}

		bot.Logger.Warn("voice connection not ready after joining", "attempt", attempt)
		if attempt < voiceReconnectAttempts {
			// Leave and try again to force a fresh session
			_ = vc.Disconnect()
			vc.Close()
			delete(bot.Session.VoiceConnections, guildID)
			time.Sleep(voiceReconnectDelay * time.Duration(attempt))
		}
	}

	return nil, errors.New("voice connection not ready after reconnection attempts")
}

// streamRadioWithFFmpeg uses ffmpeg to transcode the stream to Ogg Opus
func (bot *Bot) streamRadioWithFFmpeg(vc *discordgo.VoiceConnection, session *StreamSession, guildID, channelID string) error {
	cmd, err := bot.buildFFmpegCommand(session)
	if err != nil {
		return fmt.Errorf("build ffmpeg command: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("create stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start ffmpeg: %w", err)
	}
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Wait()
		}
	}()

	vc.Speaking(true)
	defer vc.Speaking(false)

	return bot.processStream(vc, session, bufio.NewReader(stdout), guildID, channelID)
}

// buildFFmpegCommand constructs the ffmpeg command with all arguments
func (bot *Bot) buildFFmpegCommand(session *StreamSession) (*exec.Cmd, error) {
	ffmpegFilename := ffmpegBinary
	if runtime.GOOS == "windows" {
		ffmpegFilename = ffmpegWindowsBinary
	}

	ffmpegPath, err := exec.LookPath(ffmpegFilename)
	if err != nil {
		return nil, fmt.Errorf("ffmpeg not found: %w", err)
	}

	args := []string{
		"-re",
		"-i", session.Station.StreamURL,
		"-reconnect", "1",
		"-reconnect_streamed", "1",
		"-reconnect_delay_max", "5",
		"-fflags", "+genpts+igndts",
		"-avoid_negative_ts", "make_zero",
		"-max_delay", "5000000",
		"-vn",
		"-ac", fmt.Sprintf("%d", channels),
		"-ar", fmt.Sprintf("%d", sampleRate),
		"-c:a", "libopus",
		"-b:a", bitrate,
		"-frame_duration", frameDuration,
		"-application", "audio",
		"-vbr", "off",
		"-compression_level", "10",
		"-bufsize", bufferSize,
		"-max_muxing_queue_size", fmt.Sprintf("%d", maxMuxingQueueSize),
		"-f", "ogg",
		"pipe:1",
	}

	return exec.CommandContext(session.Context, ffmpegPath, args...), nil
}

// streamRadioDirectOpus streams Ogg Opus directly from HTTP without transcoding.
func (bot *Bot) streamRadioDirectOpus(vc *discordgo.VoiceConnection, session *StreamSession, guildID, channelID string) error {
	client := bot.createHTTPClient()

	req, err := http.NewRequestWithContext(session.Context, http.MethodGet, session.Station.StreamURL, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("http error: %d %s", resp.StatusCode, resp.Status)
	}

	// Verify content type
	contentType := resp.Header.Get("Content-Type")
	if contentType != contentTypeOgg && contentType != contentTypeAudioOgg {
		bot.Logger.Warn("stream content type is not Ogg/Opus", "contentType", contentType)
		return ErrInvalidContentType
	}

	vc.Speaking(true)
	defer vc.Speaking(false)

	reader := bufio.NewReaderSize(resp.Body, oggReaderBufferSize)
	return bot.processStream(vc, session, reader, guildID, channelID)
}

// createHTTPClient creates an HTTP client optimized for streaming
func (bot *Bot) createHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 0,
		Transport: &http.Transport{
			ReadBufferSize:        httpReadBufferSize,
			WriteBufferSize:       httpWriteBufferSize,
			IdleConnTimeout:       httpIdleTimeout,
			TLSHandshakeTimeout:   httpDialTimeout,
			ResponseHeaderTimeout: httpDialTimeout,
			DialContext: (&net.Dialer{
				Timeout:   httpDialTimeout,
				KeepAlive: httpKeepAlive,
			}).DialContext,
		},
	}
}

// processStream is a common handler for both ffmpeg and direct streaming
func (bot *Bot) processStream(vc *discordgo.VoiceConnection, session *StreamSession, reader *bufio.Reader, guildID, channelID string) error {
	packets := make(chan []byte, packetBufferSize)
	errChan := make(chan error, 1)

	go bot.parseOggOpusStream(session.Context, reader, packets, errChan)

	return bot.sendOpusPackets(vc, session, packets, errChan, guildID, channelID)
}
