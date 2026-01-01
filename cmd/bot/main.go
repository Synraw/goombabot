package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/synraw/goombabot/internal/config"
	"github.com/synraw/goombabot/internal/discord"
)

func main() {
	// Open log file for writing (create if doesn't exist, append if does)
	// Ensure logs directory exists
	_ = os.MkdirAll("./logs", 0755)
	logFile, err := os.OpenFile("./logs/goombabot.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		log.Fatalf("Failed to open log file: %v", err)
	}
	defer logFile.Close()

	// Create a multi-writer that writes to both stdout and file
	multiWriter := io.MultiWriter(os.Stdout, logFile)

	// Setup logging (both slog & log global loggers can be used)
	logger := slog.New(slog.NewJSONHandler(multiWriter, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
	slog.SetDefault(logger)

	// Load .env file if present (windows workaround)
	godotenv.Load(".env")

	// Load configuration from environment variables
	cfg, err := config.LoadConfig()
	if err != nil {
		panic(err)
	}

	// Verify ffmpeg is available
	_, err = exec.LookPath("ffmpeg")
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

	// Shared shutdown context driven by OS signals
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Initialize Discord bot
	bot, err := discord.New(cfg.DiscordToken, logger, cfg)
	if err != nil {
		log.Fatalf("Failed to create Discord bot: %v", err)
	}

	botErr := make(chan error, 1)
	go func() {
		botErr <- bot.Start(ctx)
	}()

	// Expose prometheus metrics
	http.Handle("/metrics", promhttp.Handler())
	srv := &http.Server{
		Addr: fmt.Sprintf(":%d", cfg.MetricsPort),
	}

	srvErr := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			srvErr <- err
			return
		}
		srvErr <- nil
	}()

	var runErr error
	select {
	case runErr = <-botErr:
	case runErr = <-srvErr:
	case <-ctx.Done():
	}

	// Cancel everything and shut down HTTP server gracefully
	stop()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Server shutdown failed: %+v", err)
	}

	if runErr != nil {
		log.Fatalf("Service error: %v", runErr)
	}
	log.Println("Server exited gracefully")
}
