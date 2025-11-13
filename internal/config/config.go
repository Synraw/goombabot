package config

import "github.com/kelseyhightower/envconfig"

type Config struct {
	ServiceName    string `envconfig:"SERVICE_NAME" default:"goomba-bot"`
	MetricsPort    int    `envconfig:"METRICS_PORT" default:"8080"`
	DiscordToken   string `envconfig:"DISCORD_TOKEN" default:""`
	AzurecastToken string `envconfig:"AZURECAST_TOKEN" default:""`
	AzurecastApiUrl string `envconfig:"AZURECAST_API_URL" default:""`
}

func LoadConfig() (*Config, error) {
	var c Config
	err := envconfig.Process("", &c)
	if err != nil {
		return nil, err
	}
	return &c, nil
}