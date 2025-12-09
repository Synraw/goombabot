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
	maxConsecutiveEmpty = 50 // 1 second of empty ticks

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
func (bot *Bot) sendOpusPackets(vc *discordgo.VoiceConnection, session *StreamSession, packets <-chan []byte, errChan <-chan error) error {
	ringBuffer := make([][]byte, 0, maxBufferSize)

	// Fill initial buffer
	if err := bot.fillInitialBuffer(session.Context, &ringBuffer, packets, errChan); err != nil {
		bot.Logger.Debug("initial buffer fill failed", "err", err)
		return err
	}

	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	var stats streamStats
	consecutiveEmpty := 0
	lastOverflowLog := time.Time{}

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
					bot.Logger.Error("buffer starved", "count", consecutiveEmpty)
					bot.logStreamStats(&stats)
					return ErrBufferStarvation
				}
				continue
			}
			consecutiveEmpty = 0

			if err := bot.sendNextPacket(vc, session.Context, &ringBuffer, &stats); err != nil {
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

// streamStats tracks streaming metrics
type streamStats struct {
	totalPacketsReceived int
	totalPacketsSent     int
	packetsDropped       int
	sendTimeouts         int
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
	bot.Logger.Debug("initial buffer filled", "size", len(*ringBuffer))
	return nil
}

// sendNextPacket sends the next packet from the buffer to Discord
func (bot *Bot) sendNextPacket(vc *discordgo.VoiceConnection, ctx context.Context, ringBuffer *[][]byte, stats *streamStats) error {
	packet := (*ringBuffer)[0]
	*ringBuffer = (*ringBuffer)[1:]

	if len(packet) == 0 {
		return nil
	}

	select {
	case vc.OpusSend <- packet:
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
				"opusSendChanLen", len(vc.OpusSend))
		}

		if stats.sendTimeouts >= 100 {
			bot.Logger.Error("opus channel consistently blocked, likely disconnected",
				"timeouts", stats.sendTimeouts,
				"opusChanLen", len(vc.OpusSend))
			return errors.New("opus send channel blocked")
		}
		stats.sendTimeouts = 0 // Reset to allow continued attempts
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

// streamRadioWithFFmpeg uses ffmpeg to transcode the stream to Ogg Opus
func (bot *Bot) streamRadioWithFFmpeg(vc *discordgo.VoiceConnection, session *StreamSession) error {
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

	return bot.processStream(vc, session, bufio.NewReader(stdout))
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
func (bot *Bot) streamRadioDirectOpus(vc *discordgo.VoiceConnection, session *StreamSession) error {
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
	return bot.processStream(vc, session, reader)
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
func (bot *Bot) processStream(vc *discordgo.VoiceConnection, session *StreamSession, reader *bufio.Reader) error {
	packets := make(chan []byte, packetBufferSize)
	errChan := make(chan error, 1)

	go bot.parseOggOpusStream(session.Context, reader, packets, errChan)

	return bot.sendOpusPackets(vc, session, packets, errChan)
}
