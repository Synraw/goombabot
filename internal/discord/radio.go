package discord

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"runtime"
	"time"

	"github.com/bwmarrin/discordgo"
)

const (
	packetBufferSize  = 2000                   // Increased from 800
	initialBufferSize = 300                    // Increased from 150 (6 seconds)
	maxBufferSize     = 800                    // Increased from 400 (16 seconds)
	tickInterval      = 20 * time.Millisecond  // 20ms per Opus frame
	opusSendTimeout   = 100 * time.Millisecond // timeout for sending to Discord
)

// streamRadioWithFFmpeg uses ffmpeg to transcode the stream to Ogg Opus
// and sends individual Opus packets to Discord (vc.OpusSend expects []byte).
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

	go func() {
		defer close(packets)
		var pending []byte
		invalidPacketCount := 0
		const maxInvalidPackets = 10

		for {
			select {
			case <-session.Context.Done():
				return
			default:
			}
			header := make([]byte, 27)
			if _, err := io.ReadFull(reader, header); err != nil {
				if err != io.EOF {
					bot.Logger.Error("ffmpeg/ogg read header error", "err", err)
					errChan <- err
				}
				return
			}
			if string(header[0:4]) != "OggS" {
				invalidPacketCount++
				if invalidPacketCount > maxInvalidPackets {
					bot.Logger.Error("too many invalid Ogg pages, aborting")
					return
				}
				bot.Logger.Warn("invalid Ogg page, skipping")
				continue
			}
			invalidPacketCount = 0 // Reset on valid header

			segCount := int(header[26])
			lacingVals := make([]byte, segCount)
			if _, err := io.ReadFull(reader, lacingVals); err != nil {
				bot.Logger.Error("ffmpeg/ogg read lacing error", "err", err)
				return
			}
			pageSize := 0
			for _, v := range lacingVals {
				pageSize += int(v)
			}
			pageData := make([]byte, pageSize)
			if _, err := io.ReadFull(reader, pageData); err != nil {
				bot.Logger.Error("ffmpeg/ogg read page data error", "err", err)
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
					// Validate Opus packet - minimum size is 1 byte, no strict maximum
					if len(packet) == 0 {
						bot.Logger.Warn("empty opus packet, skipping")
						continue
					}
					frame := make([]byte, len(packet))
					copy(frame, packet)
					select {
					case packets <- frame:
					case <-session.Context.Done():
						return
					case <-time.After(100 * time.Millisecond):
						// If channel is full for 100ms, log it but keep trying
						bot.Logger.Warn("packet channel congestion", "channelLen", len(packets))
						select {
						case packets <- frame:
						case <-session.Context.Done():
							return
						}
					}
				}
			}
		}
	}()

	// Use a ring buffer or slice for continuous buffering
	ringBuffer := make([][]byte, 0, maxBufferSize)

	// Fill initial buffer
	for len(ringBuffer) < initialBufferSize {
		select {
		case <-session.Context.Done():
			return nil
		case pkt, ok := <-packets:
			if !ok {
				return errors.New("ffmpeg exited before buffer filled")
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
		case pkt, ok := <-packets:
			if !ok {
				// Stream ended unexpectedly - this triggers reconnection
				bot.Logger.Warn("packet channel closed unexpectedly")
				return errors.New("stream ended unexpectedly")
			}
			ringBuffer = append(ringBuffer, pkt)
			if len(ringBuffer) > maxBufferSize {
				ringBuffer = ringBuffer[1:] // Remove oldest
			}
			consecutiveEmptyCount = 0
		case <-ticker.C:
			if len(ringBuffer) > 0 {
				packet := ringBuffer[0]
				if len(packet) > 0 {
					select {
					case vc.OpusSend <- packet:
						ringBuffer = ringBuffer[1:]
						consecutiveEmptyCount = 0
					case <-time.After(opusSendTimeout):
						bot.Logger.Warn("opus send timeout", "bufferLen", len(ringBuffer))
						ringBuffer = ringBuffer[1:]
					case <-session.Context.Done():
						return nil
					}
				} else {
					bot.Logger.Warn("dropping empty packet")
					ringBuffer = ringBuffer[1:]
				}
			} else {
				consecutiveEmptyCount++
				if consecutiveEmptyCount >= maxConsecutiveEmpty {
					// Buffer starvation for too long - force reconnection
					bot.Logger.Error("buffer starved, stream appears dead",
						"count", consecutiveEmptyCount,
						"packetsChannelLen", len(packets))
					return errors.New("buffer starvation detected")
				}
				if consecutiveEmptyCount%10 == 0 && consecutiveEmptyCount > 0 {
					// Log every 200ms during starvation
					bot.Logger.Warn("buffer empty",
						"count", consecutiveEmptyCount,
						"packetsChannelLen", len(packets),
						"ringBufferLen", len(ringBuffer))
				}
			}
		}
	}
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
			ResponseHeaderTimeout: 10 * time.Second, // Only initial response timeout
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

	// Check content type to ensure it's Ogg/Opus
	contentType := resp.Header.Get("Content-Type")
	bot.Logger.Info("streaming direct opus", "contentType", contentType)

	vc.Speaking(true)
	defer vc.Speaking(false)

	reader := bufio.NewReaderSize(resp.Body, 128*1024) // Increase to 128KB buffer
	packets := make(chan []byte, packetBufferSize)
	errChan := make(chan error, 1)

	go func() {
		defer close(packets)
		var pending []byte
		invalidPacketCount := 0
		const maxInvalidPackets = 10

		for {
			select {
			case <-session.Context.Done():
				return
			default:
			}
			header := make([]byte, 27)
			if _, err := io.ReadFull(reader, header); err != nil {
				if err != io.EOF {
					bot.Logger.Error("http/ogg read header error", "err", err)
					errChan <- err
				}
				return
			}
			if string(header[0:4]) != "OggS" {
				invalidPacketCount++
				if invalidPacketCount > maxInvalidPackets {
					bot.Logger.Error("too many invalid Ogg pages, aborting")
					return
				}
				bot.Logger.Warn("invalid Ogg page, skipping")
				continue
			}
			invalidPacketCount = 0 // Reset on valid header

			segCount := int(header[26])
			lacingVals := make([]byte, segCount)
			if _, err := io.ReadFull(reader, lacingVals); err != nil {
				bot.Logger.Error("http/ogg read lacing error", "err", err)
				return
			}
			pageSize := 0
			for _, v := range lacingVals {
				pageSize += int(v)
			}
			pageData := make([]byte, pageSize)
			if _, err := io.ReadFull(reader, pageData); err != nil {
				bot.Logger.Error("http/ogg read page data error", "err", err)
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
					// Validate Opus packet - minimum size is 1 byte, no strict maximum
					if len(packet) == 0 {
						bot.Logger.Warn("empty opus packet, skipping")
						continue
					}
					frame := make([]byte, len(packet))
					copy(frame, packet)
					select {
					case packets <- frame:
					case <-session.Context.Done():
						return
					case <-time.After(100 * time.Millisecond):
						// If channel is full for 100ms, log it but keep trying
						bot.Logger.Warn("packet channel congestion", "channelLen", len(packets))
						select {
						case packets <- frame:
						case <-session.Context.Done():
							return
						}
					}
				}
			}
		}
	}()

	// Use a ring buffer or slice for continuous buffering
	ringBuffer := make([][]byte, 0, maxBufferSize)

	// Fill initial buffer
	for len(ringBuffer) < initialBufferSize {
		select {
		case <-session.Context.Done():
			return nil
		case pkt, ok := <-packets:
			if !ok {
				return errors.New("http stream exited before buffer filled")
			}
			ringBuffer = append(ringBuffer, pkt)
		}
	}

	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	consecutiveEmptyCount := 0
	maxConsecutiveEmpty := 50 // Match ffmpeg version - 1 second of empty ticks

	for {
		select {
		case <-session.Context.Done():
			return nil
		case err := <-errChan:
			return err
		case pkt, ok := <-packets:
			if !ok {
				// Stream ended unexpectedly - this triggers reconnection
				bot.Logger.Warn("packet channel closed unexpectedly")
				return errors.New("stream ended unexpectedly")
			}
			ringBuffer = append(ringBuffer, pkt)
			if len(ringBuffer) > maxBufferSize {
				ringBuffer = ringBuffer[1:] // Remove oldest
			}
			consecutiveEmptyCount = 0
		case <-ticker.C:
			if len(ringBuffer) > 0 {
				packet := ringBuffer[0]
				if len(packet) > 0 {
					select {
					case vc.OpusSend <- packet:
						ringBuffer = ringBuffer[1:]
						consecutiveEmptyCount = 0
					case <-time.After(opusSendTimeout):
						bot.Logger.Warn("opus send timeout", "bufferLen", len(ringBuffer))
						ringBuffer = ringBuffer[1:]
					case <-session.Context.Done():
						return nil
					}
				} else {
					bot.Logger.Warn("dropping empty packet")
					ringBuffer = ringBuffer[1:]
				}
			} else {
				consecutiveEmptyCount++
				if consecutiveEmptyCount >= maxConsecutiveEmpty {
					// Buffer starvation for too long - force reconnection
					bot.Logger.Error("buffer starved, stream appears dead",
						"count", consecutiveEmptyCount,
						"packetsChannelLen", len(packets))
					return errors.New("buffer starvation detected")
				}
				if consecutiveEmptyCount%10 == 0 && consecutiveEmptyCount > 0 {
					// Log every 200ms during starvation
					bot.Logger.Warn("buffer empty",
						"count", consecutiveEmptyCount,
						"packetsChannelLen", len(packets),
						"ringBufferLen", len(ringBuffer))
				}
			}
		}
	}
}
