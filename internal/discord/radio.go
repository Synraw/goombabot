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
	packetBufferSize   = 2000
	initialBufferSize  = 150 // Increase from 100 -> 3 seconds (more headroom)
	maxBufferSize      = 400 // Increase from 300 -> 8 seconds maximum
	tickInterval       = 20 * time.Millisecond
	opusSendTimeout    = 500 * time.Millisecond
	maxInvalidOggPages = 10
)

// parseOggOpusStream reads Ogg pages from a reader and sends Opus packets to a channel.
func (bot *Bot) parseOggOpusStream(ctx context.Context, reader *bufio.Reader, packets chan<- []byte, errChan chan<- error) {
	defer close(packets)
	var pending []byte
	invalidPacketCount := 0

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		header := make([]byte, 27)
		if _, err := io.ReadFull(reader, header); err != nil {
			if err != io.EOF && err != io.ErrUnexpectedEOF {
				bot.Logger.Error("ogg read header error", "err", err, "headerData", hex.EncodeToString(header))
				select {
				case errChan <- fmt.Errorf("ogg header read failed: %w", err):
				default:
				}
			}
			return
		}
		if string(header[0:4]) != "OggS" {
			invalidPacketCount++
			if invalidPacketCount > maxInvalidOggPages {
				bot.Logger.Error("too many invalid Ogg pages, aborting")
				return
			}
			bot.Logger.Warn("invalid ogg page, skipping")
			continue
		}
		invalidPacketCount = 0 // Reset on valid header

		segCount := int(header[26])
		lacingVals := make([]byte, segCount)
		if _, err := io.ReadFull(reader, lacingVals); err != nil {
			bot.Logger.Error("ogg read lacing error", "err", err)
			return
		}
		pageSize := 0
		for _, v := range lacingVals {
			pageSize += int(v)
		}
		pageData := make([]byte, pageSize)
		if _, err := io.ReadFull(reader, pageData); err != nil {
			bot.Logger.Error("ogg read page data error", "err", err)
			return
		}
		offset := 0
		for _, lv := range lacingVals {
			size := int(lv)
			if size == 0 {
				continue
			}
			if offset+size > len(pageData) {
				bot.Logger.Warn("segment overflow, skipping rest of page")
				break
			}
			seg := pageData[offset : offset+size]
			offset += size
			pending = append(pending, seg...)
			if size < 255 {
				packet := pending
				pending = nil
				if len(packet) >= 8 {
					if string(packet[:8]) == "OpusHead" || string(packet[:8]) == "OpusTags" {
						continue
					}
				}
				// Validate Opus packet - minimum size is 1 byte
				if len(packet) == 0 {
					bot.Logger.Warn("empty opus packet, skipping")
					continue
				}
				frame := make([]byte, len(packet))
				copy(frame, packet)
				select {
				case packets <- frame:
				case <-ctx.Done():
					return
				case <-time.After(100 * time.Millisecond):
					// If channel is full for 100ms, log it but keep trying
					bot.Logger.Warn("packet channel congestion", "channelLen", len(packets))
					select {
					case packets <- frame:
					case <-ctx.Done():
						return
					}
				}
			}
		}
	}
}

// sendOpusPackets handles buffering and sending Opus packets to Discord.
func (bot *Bot) sendOpusPackets(vc *discordgo.VoiceConnection, session *StreamSession, packets <-chan []byte, errChan <-chan error) error {
	// Use a ring buffer for continuous buffering
	ringBuffer := make([][]byte, 0, maxBufferSize)

	// Fill initial buffer
	for len(ringBuffer) < initialBufferSize {
		select {
		case <-session.Context.Done():
			return nil
		case pkt, ok := <-packets:
			if !ok {
				return errors.New("stream exited before buffer filled")
			}
			ringBuffer = append(ringBuffer, pkt)
		}
	}

	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	consecutiveEmptyCount := 0
	maxConsecutiveEmpty := 50 // 1 second of empty ticks

	for {
		select {
		case <-session.Context.Done():
			return nil
		case err := <-errChan:
			return err
		case <-ticker.C:
			// Try to drain excess packets from channel before sending
			drainedCount := 0
			for len(packets) > packetBufferSize*3/4 && drainedCount < 50 {
				select {
				case pkt := <-packets:
					ringBuffer = append(ringBuffer, pkt)
					drainedCount++
				default:
					break
				}
			}

			// Dynamic buffer management - keep it reasonable
			if len(ringBuffer) > maxBufferSize {
				dropCount := len(ringBuffer) - maxBufferSize
				bot.Logger.Warn("ring buffer overflow", "dropping", dropCount, "packetsWaiting", len(packets))
				ringBuffer = ringBuffer[dropCount:]
			}

			if len(ringBuffer) > 0 {
				packet := ringBuffer[0]
				ringBuffer = ringBuffer[1:] // Always remove, even if empty

				if len(packet) > 0 {
					select {
					case vc.OpusSend <- packet:
						consecutiveEmptyCount = 0
					case <-time.After(opusSendTimeout):
						bot.Logger.Warn("opus send timeout", "bufferLen", len(ringBuffer), "packetsWaiting", len(packets))
						// Don't return error, just skip this packet
						consecutiveEmptyCount++
					case <-session.Context.Done():
						return nil
					}
				}
			} else {
				// Buffer empty - try to pull one packet immediately
				select {
				case pkt, ok := <-packets:
					if !ok {
						bot.Logger.Warn("packet channel closed unexpectedly")
						return errors.New("stream ended unexpectedly")
					}
					ringBuffer = append(ringBuffer, pkt)
				default:
					consecutiveEmptyCount++
					if consecutiveEmptyCount >= maxConsecutiveEmpty {
						// Buffer starvation for too long - force reconnection
						bot.Logger.Error("buffer starved, stream appears dead",
							"count", consecutiveEmptyCount,
							"packetsChannelLen", len(packets))
						return errors.New("buffer starvation detected")
					}
				}
			}
		}
	}
}

// streamRadioWithFFmpeg uses ffmpeg to transcode the stream to Ogg Opus
func (bot *Bot) streamRadioWithFFmpeg(vc *discordgo.VoiceConnection, session *StreamSession) error {
	ffmpegFilename := "ffmpeg"
	if runtime.GOOS == "windows" {
		ffmpegFilename = ".\\ffmpeg.exe"
	}
	ffmpegPath, err := exec.LookPath(ffmpegFilename)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(session.Context, ffmpegPath,
		"-re",                           // Read input at native frame rate
		"-i", session.Station.StreamURL, // Input URL
		"-reconnect", "1", // Enable reconnection
		"-reconnect_streamed", "1", // Reconnect for streamed files
		"-reconnect_delay_max", "5", // Max delay of 5 seconds
		"-fflags", "+genpts+igndts", // Generate PTS and ignore DTS discontinuities
		"-avoid_negative_ts", "make_zero", // Handle negative timestamps
		"-max_delay", "5000000", // 5 seconds max delay
		"-vn",      // No video
		"-ac", "2", // 2 audio channels
		"-ar", "48000", // 48kHz sample rate
		"-c:a", "libopus", // Encode to Opus
		"-b:a", "128k", // 128kbps bitrate
		"-frame_duration", "20", // 20ms frames
		"-application", "audio", // Audio application
		"-vbr", "off", // Disable VBR for consistent quality
		"-compression_level", "10", // Maximum quality (0-10)
		"-bufsize", "256k", // Buffer size
		"-max_muxing_queue_size", "1024", // Increase muxing queue size
		"-f", "ogg", // Output format Ogg
		"pipe:1", // Output to stdout
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}

	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Wait()
		}
	}()

	vc.Speaking(true)
	defer vc.Speaking(false)

	reader := bufio.NewReader(stdout)
	packets := make(chan []byte, packetBufferSize)
	errChan := make(chan error, 1)

	go bot.parseOggOpusStream(session.Context, reader, packets, errChan)

	return bot.sendOpusPackets(vc, session, packets, errChan)
}

// streamRadioDirectOpus streams Ogg Opus directly from HTTP without transcoding.
func (bot *Bot) streamRadioDirectOpus(vc *discordgo.VoiceConnection, session *StreamSession) error {
	client := &http.Client{
		Timeout: 0, // No timeout for streaming connections
		Transport: &http.Transport{
			ReadBufferSize:        256 * 1024,
			WriteBufferSize:       256 * 1024,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 10 * time.Second,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		},
	}

	req, err := http.NewRequestWithContext(session.Context, "GET", session.Station.StreamURL, nil)
	if err != nil {
		return err
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("http error: %d %s", resp.StatusCode, resp.Status)
	}

	// Verify content type is Ogg/Opus
	contentType := resp.Header.Get("Content-Type")
	if contentType != "application/ogg" && contentType != "audio/ogg" {
		bot.Logger.Warn("stream content type is not Ogg/Opus", "contentType", contentType)
		return errors.New("stream is not in Ogg/Opus format")
	}

	vc.Speaking(true)
	defer vc.Speaking(false)

	// Create buffered reader with increased buffer size
	reader := bufio.NewReaderSize(resp.Body, 128*1024)
	packets := make(chan []byte, packetBufferSize)
	errChan := make(chan error, 1)

	// Parse Ogg/Opus stream
	go bot.parseOggOpusStream(session.Context, reader, packets, errChan)

	// Send Opus packets to Discord
	return bot.sendOpusPackets(vc, session, packets, errChan)
}
