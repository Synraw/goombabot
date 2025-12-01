package discord

import (
	"bufio"
	"errors"
	"io"
	"os/exec"
	"runtime"
	"time"

	"github.com/bwmarrin/discordgo"
)

const (
	packetBufferSize  = 800
	initialBufferSize = 150
	maxBufferSize     = 400
	tickInterval      = 20 * time.Millisecond
	opusSendTimeout   = 100 * time.Millisecond
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
		"-re",
		"-i", session.Station.StreamURL,
		"-reconnect", "1",
		"-reconnect_streamed", "1",
		"-reconnect_delay_max", "5",
		"-vn",
		"-ac", "2",
		"-ar", "48000",
		"-c:a", "libopus",
		"-b:a", "96k",
		"-frame_duration", "20",
		"-application", "audio",
		"-compression_level", "10",
		"-packet_loss", "15",
		"-vbr", "on",
		"-bufsize", "192k",
		"-max_muxing_queue_size", "1024",
		"-f", "ogg",
		"pipe:1",
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}

	// Set stderr to capture ffmpeg errors
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}

	// Log ffmpeg errors in background
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			bot.Logger.Debug("ffmpeg", "output", scanner.Text())
		}
	}()

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
				bot.Logger.Error("not an Ogg page, aborting")
				return
			}
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
					return
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
					frame := make([]byte, len(packet))
					copy(frame, packet)
					select {
					case packets <- frame:
					case <-session.Context.Done():
						return
					default:
						<-packets
						packets <- frame
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
	maxConsecutiveEmpty := 5

	for {
		select {
		case <-session.Context.Done():
			return nil
		case err := <-errChan:
			return err
		case pkt, ok := <-packets:
			if !ok {
				// Stream ended, drain remaining buffer
				for len(ringBuffer) > 0 {
					select {
					case vc.OpusSend <- ringBuffer[0]:
						ringBuffer = ringBuffer[1:]
						time.Sleep(tickInterval)
					case <-session.Context.Done():
						return nil
					}
				}
				return nil
			}
			ringBuffer = append(ringBuffer, pkt)
			if len(ringBuffer) > maxBufferSize {
				ringBuffer = ringBuffer[1:] // Remove oldest
			}
			consecutiveEmptyCount = 0 // Reset on packet arrival
		case <-ticker.C:
			if len(ringBuffer) > 0 {
				select {
				case vc.OpusSend <- ringBuffer[0]:
					ringBuffer = ringBuffer[1:]
					consecutiveEmptyCount = 0
				case <-time.After(opusSendTimeout):
					bot.Logger.Warn("opus send timeout", "bufferLen", len(ringBuffer))
				case <-session.Context.Done():
					return nil
				}
			} else {
				consecutiveEmptyCount++
				if consecutiveEmptyCount >= maxConsecutiveEmpty {
					bot.Logger.Warn("buffer empty for too long",
						"count", consecutiveEmptyCount,
						"packetsChannelLen", len(packets))
					consecutiveEmptyCount = 0 // Reset to avoid spam
				}
			}
		}
	}
}
