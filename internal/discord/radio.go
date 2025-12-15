package discord

import (
	"encoding/binary"
	"io"
	"os/exec"
	"runtime"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	gopus "layeh.com/gopus"
)

const (
	pcmSampleRate       = 48000
	pcmChannels         = 2
	opusFrameMillis     = 20   // 20ms frames are standard for Discord
	initialBufferFrames = 300  // ~6s; WSL2 I/O needs bigger buffer
	maxBufferFrames     = 1500 // WSL2 can be unpredictable; need more headroom
	opusSendWarnTimeout = 200 * time.Millisecond
	startBufferTimeout  = 6 * time.Second // WSL2 startup is slower
)

// streamRadio uses ffmpeg to decode the remote stream to raw PCM and encodes
// it to Opus frames with gopus, sending directly to Discord. This keeps the
// pipeline simple and avoids custom Ogg parsing and large buffering.
func (bot *Bot) streamRadio(vc *discordgo.VoiceConnection, session *StreamSession) error {
	// Prepare ffmpeg command to output signed 16-bit little-endian PCM at 48kHz stereo
	ffmpegBin := "ffmpeg"
	if runtime.GOOS == "windows" {
		ffmpegBin = ".\\ffmpeg.exe"
	}

	// Do NOT use -re; we control pacing with a ticker and buffer
	// WSL2 I/O is unpredictable. Use ffmpeg's internal buffering to smooth the stream,
	// and let our Go buffer absorb timing jitter from the virtual I/O layer.
	var afiltergraph string
	var extraArgs []string
	if runtime.GOOS == "linux" {
		// WSL2 workaround: use native resampler without async (simpler, fewer context switches)
		// and let ffmpeg do more buffering on its end to smooth network/WSL jitter
		afiltergraph = "aresample=48000"
		extraArgs = []string{
			"-fflags", "+nobuffer+genpts",
			"-thread_queue_size", "256",
			"-buffer_size", "2M", // Larger ffmpeg buffer for WSL2 unpredictability
		}
	} else {
		// Windows: async resampler smooths timing
		afiltergraph = "aresample=async=1:min_hard_comp=0.1:first_pts=0"
	}

	args := []string{
		"-loglevel", "warning",
		"-thread_queue_size", "512",
		"-i", session.Station.StreamURL,
		"-reconnect", "1",
		"-reconnect_streamed", "1",
		"-reconnect_delay_max", "5",
		"-fflags", "+genpts",
		"-vn",
		// Audio conditioning: use OS-specific resampler config
		"-af", afiltergraph,
		"-ac", "2",
		"-ar", "48000",
		"-f", "s16le",
		"-acodec", "pcm_s16le",
		"pipe:1",
	}

	// Add Linux-specific ffmpeg optimizations
	if runtime.GOOS == "linux" {
		args = append(args[:len(args)-1], extraArgs...)
		args = append(args, "-f", "s16le", "-acodec", "pcm_s16le", "pipe:1")
	}

	cmd := exec.CommandContext(session.Context, ffmpegBin, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() { _ = cmd.Wait() }()

	// Opus encoder setup
	enc, err := gopus.NewEncoder(pcmSampleRate, pcmChannels, gopus.Audio)
	if err != nil {
		return err
	}
	// Configure encoder for stable, CBR-like output
	// Note: older gopus APIs use void setters (no error return) and expose
	// fewer tuning knobs. Use what's available for this version.
	enc.SetBitrate(128000)
	enc.SetVbr(false)

	// Calculate PCM frame parameters
	frameSamples := pcmSampleRate / (1000 / opusFrameMillis) // 960 samples per channel at 48kHz/20ms
	samplesPerFrame := frameSamples * pcmChannels            // 1920 samples total
	bytesPerFrame := samplesPerFrame * 2                     // s16le: 2 bytes per sample

	// Read directly from stdout without Go buffering; ffmpeg's -fflags +nobuffer
	// ensures it flushes immediately. Buffering layer here would reintroduce stalls.

	frames := make(chan []byte, maxBufferFrames)
	var wg sync.WaitGroup
	done := make(chan struct{})

	// Producer: read PCM frames and encode to Opus
	// Use a dedicated reader with timeout to prevent blocking on slow ffmpeg
	wg.Add(1)
	go func() {
		defer wg.Done()
		pcmBuf := make([]byte, bytesPerFrame)
		int16Buf := make([]int16, samplesPerFrame)
		framesRead := 0

		for {
			select {
			case <-session.Context.Done():
				return
			default:
			}

			// Read PCM with timeout to avoid indefinite blocking
			// Use io.ReadAtLeast to read exactly what we need; avoids partial reads
			type readResult struct {
				n   int
				err error
			}
			readChan := make(chan readResult, 1)
			go func() {
				n, err := io.ReadAtLeast(stdout, pcmBuf, bytesPerFrame)
				readChan <- readResult{n, err}
			}()

			select {
			case result := <-readChan:
				if result.err != nil {
					close(done)
					return
				}
				// io.ReadAtLeast guarantees >= bytesPerFrame bytes
			case <-session.Context.Done():
				return
			case <-time.After(2 * time.Second):
				// ffmpeg pipe stalled for 2 seconds; assume it died
				bot.Logger.Warn("ffmpeg pipe stalled; ending stream")
				close(done)
				return
			}

			// Convert bytes to int16 samples
			for i := 0; i < samplesPerFrame; i++ {
				int16Buf[i] = int16(binary.LittleEndian.Uint16(pcmBuf[i*2 : i*2+2]))
			}

			// Encode to Opus
			opus, err := enc.Encode(int16Buf, frameSamples, bytesPerFrame)
			if err != nil {
				close(done)
				return
			}

			framesRead++

			// Drop oldest if buffer full; keep most recent frames
			select {
			case frames <- opus:
			default:
				select {
				case <-frames:
				default:
				}
				select {
				case frames <- opus:
				default:
					// Even dropping one didn't help; skip this frame
				}
			}
		}
	}()

	vc.Speaking(true)
	defer vc.Speaking(false)

	// Wait for initial buffer or timeout
	start := time.Now()
	for len(frames) < initialBufferFrames {
		select {
		case <-session.Context.Done():
			close(frames)
			wg.Wait()
			return nil
		case <-done:
			// ffmpeg ended before buffer filled; continue with what we have
			break
		case <-time.After(10 * time.Millisecond):
		}
		if time.Since(start) > startBufferTimeout {
			break
		}
	}

	// Stable pacing scheduler using ticker
	frameDur := time.Duration(opusFrameMillis) * time.Millisecond
	ticker := time.NewTicker(frameDur)
	defer ticker.Stop()

	emptyCount := 0
	framesSent := 0
	skippedFrames := 0

	for {
		select {
		case <-session.Context.Done():
			close(frames)
			wg.Wait()
			return nil

		case <-ticker.C:
			// Ensure voice is ready
			if vc == nil || vc.OpusSend == nil || !vc.Ready {
				continue
			}

			// Get a frame if available; if not, skip this tick to prevent jitter
			// (WSL2 will catch up on the next few ticks)
			var frame []byte
			select {
			case frame = <-frames:
				emptyCount = 0
				framesSent++
			default:
				emptyCount++
				skippedFrames++
				// Log underruns less frequently (WSL2 jitter may cause frequent ones)
				if emptyCount%200 == 0 {
					bot.Logger.Warn("audio buffer underrun (WSL2 jitter)",
						"frames_queued", len(frames),
						"frames_sent", framesSent,
						"skipped", skippedFrames)
				}
				continue
			}

			// Send with timeout
			select {
			case vc.OpusSend <- frame:
			case <-time.After(opusSendWarnTimeout):
				bot.Logger.Warn("opus send delayed; Discord backpressure")
			}

		case <-done:
			// Source finished; drain remaining frames and exit
			for {
				select {
				case frame := <-frames:
					if vc != nil && vc.OpusSend != nil && vc.Ready {
						select {
						case vc.OpusSend <- frame:
						case <-time.After(opusSendWarnTimeout):
						}
					}
				default:
					wg.Wait()
					return nil
				}
			}
		}
	}
}
