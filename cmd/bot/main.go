package main

import (
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/synraw/goombabot/internal/config"
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

	// Expose prometheus metrics
	http.Handle("/metrics", promhttp.Handler())
	err = http.ListenAndServe(fmt.Sprintf(":%d", cfg.MetricsPort), nil)
	if err != nil {
		log.Fatal(err)
	}
}
