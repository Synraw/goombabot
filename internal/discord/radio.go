package discord

import (
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
	pcmSampleRate       = 48000                  // Discord standard
	pcmChannels         = 2                      // stereo
	opusFrameMillis     = 20                     // 20ms frames are standard for Discord
	initialBufferFrames = 100                    // ~2s initial buffer to smooth jitter
	maxBufferFrames     = 1500                   // ~30s max to avoid excessive memory use
	opusSendWarnTimeout = 200 * time.Millisecond // warn if send blocks this long
	startBufferTimeout  = 3 * time.Second        // max wait for initial buffer

	// global constants
	DefaultVolume = 1.0 // default volume multiplier
)

// goWait starts fn in a goroutine and tracks it with the WaitGroup.
func goWait(wg *sync.WaitGroup, fn func()) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		fn()
	}()
}

// Add this helper function to apply volume
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

// streamRadio uses ffmpeg to decode the remote stream to raw PCM and encodes
// it to Opus frames with gopus, sending directly to Discord. This keeps the
// pipeline simple and avoids custom Ogg parsing and large buffering.
func (bot *Bot) streamRadio(vc *discordgo.VoiceConnection, session *StreamSession) error {
	// Prepare ffmpeg command to output signed 16-bit little-endian PCM at 48kHz stereo
	ffmpegBin := "ffmpeg"
	if runtime.GOOS == "windows" {
		ffmpegBin = ".\\ffmpeg.exe"
	}

	var afiltergraph string
	var extraArgs []string
	if runtime.GOOS == "linux" {
		afiltergraph = "aresample=48000"
		extraArgs = []string{
			"-fflags", "+nobuffer+genpts",
			"-thread_queue_size", "256",
			"-buffer_size", "2M",
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
	goWait(&wg, func() {
		pcmBuf := make([]byte, bytesPerFrame)
		int16Buf := make([]int16, samplesPerFrame)

		for {
			select {
			case <-session.Context.Done():
				return
			default:
			}

			// Read exactly one Opus frame worth of PCM; context cancellation kills ffmpeg so read returns
			if _, err := io.ReadFull(stdout, pcmBuf); err != nil {
				close(done)
				return
			}

			for i := 0; i < samplesPerFrame; i++ {
				int16Buf[i] = int16(binary.LittleEndian.Uint16(pcmBuf[i*2 : i*2+2]))
			}

			// Apply volume adjustment
			applyVolume(int16Buf, session.Volume)

			// Encode to Opus
			opus, err := enc.Encode(int16Buf, frameSamples, bytesPerFrame)
			if err != nil {
				close(done)
				return
			}

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
					// Buffer still congested; skip frame
				}
			}
		}
	})

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
