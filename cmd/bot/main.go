package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"runtime"

	"github.com/joho/godotenv"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/synraw/goombabot/internal/config"
	"github.com/synraw/goombabot/internal/discord"
)

func main() {
	// Setup logging (both slog & log global loggers can be used)
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level:     slog.LevelDebug,
		AddSource: true,
	}))
	slog.SetDefault(logger)

	godotenv.Load(".env")

	// Load configuration from environment variables
	cfg, err := config.LoadConfig()
	if err != nil {
		panic(err)
	}

	// Verify ffmpeg is available

	ffmpegFilename := "ffmpeg"
	if runtime.GOOS == "windows" {
		ffmpegFilename = "ffmpeg.exe"
	}

	_, err = exec.LookPath(ffmpegFilename)
	if err != nil {
		log.Fatalf("ffmpeg not found in current directory or PATH: %v", err)
	}

	if cfg.DiscordToken == "" {
		log.Fatal("no discord token provided in DISCORD_TOKEN env var")
	}

	if cfg.AzurecastApiUrl == "" {
		log.Fatal("no Azurecast API URL provided in AZURECAST_API_URL env var")
	}

	if cfg.AzurecastToken == "" {
		log.Fatal("no Azurecast API key provided in AZURECAST_API_KEY env var")
	}

	// Initialize Discord bot
	bot, err := discord.New(cfg.DiscordToken, logger, cfg)
	if err != nil {
		log.Fatalf("Failed to create Discord bot: %v", err)
	}

	// Start the bot in a separate goroutine
	go func() {
		if err := bot.Start(context.Background()); err != nil {
			log.Fatalf("Discord bot error: %v", err)
		}
	}()

	// Expose prometheus metrics
	http.Handle("/metrics", promhttp.Handler())
	err = http.ListenAndServe(fmt.Sprintf(":%d", cfg.MetricsPort), nil)
	if err != nil {
		log.Fatal(err)
	}
}
