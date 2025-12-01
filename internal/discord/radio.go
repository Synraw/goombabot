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
	packetBufferSize  = 400
	initialBufferSize = 50
	maxBufferSize     = 100
	tickInterval      = 15 * time.Millisecond
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
		"-vn",
		"-ac", "2",
		"-ar", "48000",
		"-c:a", "libopus",
		"-b:a", "96k",
		"-frame_duration", "20",
		"-f", "ogg",
		"pipe:1",
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
		case <-ticker.C:
			if len(ringBuffer) > 0 {
				select {
				case vc.OpusSend <- ringBuffer[0]:
					ringBuffer = ringBuffer[1:]
				case <-time.After(opusSendTimeout):
					bot.Logger.Warn("opus send timeout")
				case <-session.Context.Done():
					return nil
				}
			}
		}
	}
}
