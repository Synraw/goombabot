package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/synraw/goombabot/internal/config"
	"github.com/synraw/goombabot/internal/discord"
)

func main() {
	// Setup logging (both slog & log global loggers can be used)
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
	slog.SetDefault(logger)

	// Load configuration from environment variables
	cfg, err := config.LoadConfig()
	if err != nil {
		panic(err)
	}

	if cfg.DiscordToken == "" {
		log.Fatal("DISCORD_TOKEN is required")
	}

	// Initialize Discord bot
	bot, err := discord.New(cfg.DiscordToken, logger)
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
