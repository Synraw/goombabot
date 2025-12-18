package discord

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"

	"github.com/bwmarrin/discordgo"
	"github.com/synraw/goombabot/internal/azurecast"
	"github.com/synraw/goombabot/internal/config"
)

type CommandHandler func(b *Bot, s *discordgo.Session, i *discordgo.InteractionCreate)

type CommandDef struct {
	Command *discordgo.ApplicationCommand
	Handle  CommandHandler
}

type RadioStation struct {
	ID        int
	Name      string
	StreamURL string
	IsOpus    bool
}

type StreamSession struct {
	Context context.Context
	Cancel  context.CancelFunc
	UserID  string
	Station *RadioStation
	Volume  float64
}

type Bot struct {
	Token          string
	Session        *discordgo.Session
	Logger         *slog.Logger
	commands       map[string]CommandDef
	config         *config.Config
	azureApiClient *azurecast.Client
	radioStations  map[int]RadioStation
	radioMutex     sync.Mutex
	radioSessions  map[string]*StreamSession
	sessionStore   *SessionStore
}

// AddCommand adds a command definition to the bot.
func (b *Bot) AddCommand(
	command, description string,
	handler CommandHandler,
	options ...*discordgo.ApplicationCommandOption,
) {
	cmd := &discordgo.ApplicationCommand{
		Name:        command,
		Description: description,
		Type:        discordgo.ChatApplicationCommand,
		Options:     options,
	}
	b.commands[command] = CommandDef{
		Command: cmd,
		Handle:  handler,
	}
}

// New creates a new Discord bot instance.
func New(token string, logger *slog.Logger, cfg *config.Config) (*Bot, error) {
	sess, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, err
	}

	// Initialize session store
	sessionStore, err := NewSessionStore("./data")
	if err != nil {
		return nil, err
	}

	bot := &Bot{
		Token:         token,
		Session:       sess,
		Logger:        logger,
		config:        cfg,
		commands:      map[string]CommandDef{},
		radioStations: make(map[int]RadioStation),
		radioSessions: make(map[string]*StreamSession),
		sessionStore:  sessionStore,
	}

	// Initialize Azurecast client
	bot.azureApiClient, err = azurecast.NewClient(cfg.AzurecastApiUrl,
		azurecast.WithAPIKey(cfg.AzurecastToken),
	)
	if err != nil {
		return nil, err
	}

	// Fetch available radio stations from Azurecast
	radioStations, err := bot.azureApiClient.GetStations(context.Background())
	if err != nil {
		return nil, err
	}

	// Load radio stations into bot
	for _, station := range radioStations {
		opusMountUrl := station.ListenURL
		isOpus := false
		// Find the opus mount if available otherwise default to primary listen URL
		for _, mount := range station.Mounts {
			if mount.Format == "opus" {
				opusMountUrl = mount.URL
				isOpus = true
				break
			}
		}

		bot.Logger.Info("station found", "name", station.Name, "id", station.ID)
		bot.radioStations[station.ID] = RadioStation{
			ID:        station.ID,
			Name:      station.Name,
			StreamURL: opusMountUrl,
			IsOpus:    isOpus,
		}
	}

	bot.AddCommand("radio", "Play a station from your Azurecast server in the current voice channel", (*Bot).handleRadio)
	bot.AddCommand("stop", "Stops the currently streaming radio from playing", (*Bot).handleStop)
	bot.AddCommand("skip", "Skips the currently playing song on the radio station", (*Bot).handleSkip)
	bot.AddCommand("nowplaying", "Shows the currently playing song on the radio station", (*Bot).handleNowPlaying)
	bot.AddCommand("volume", "Set the volume for the current playing radio station", (*Bot).handleVolume,
		&discordgo.ApplicationCommandOption{
			Type:        discordgo.ApplicationCommandOptionInteger,
			Name:        "level",
			Description: "Volume level from " + strconv.Itoa(VolumeMin) + " to " + strconv.Itoa(VolumeMax) + " percent (default " + strconv.Itoa(int(DefaultVolume*100)) + "%)",
			Required:    true,
		},
	)
	bot.AddCommand("request", "Request a song to be played on the radio station (can show up-to 25 results to choose from)", (*Bot).handleRequest,
		&discordgo.ApplicationCommandOption{
			Type:        discordgo.ApplicationCommandOptionString,
			Name:        "song",
			Description: "The name of the song to request",
			Required:    true,
		},
	)

	return bot, nil
}

// Start opens the Discord session and begins listening for events.
func (b *Bot) Start(ctx context.Context) error {
	if err := b.Session.Open(); err != nil {
		return err
	}

	b.RegisterCommands()
	b.Logger.Debug("Discord bot started")
	defer b.Session.Close()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-ctx.Done():
	case <-stop:
		b.Logger.Info("Shutting down Discord bot")
		b.radioMutex.Lock()
		for guildID, session := range b.radioSessions {
			b.Logger.Debug("stopping radio stream", "guild_id", guildID)
			session.Cancel()
		}
		b.radioMutex.Unlock()
	}
	return nil
}

// RegisterCommands registers all commands with Discord.
func (b *Bot) RegisterCommands() {
	// route all interaction events to our dispatcher
	b.Session.AddHandler(b.onInteractionUpdate)
	// route voice state updates to our handler
	b.Session.AddHandler(b.onVoiceStateUpdate)
	// route events for when we join a guild
	b.Session.AddHandler(b.onGuildCreate)
	// route events for when we leave a guild
	b.Session.AddHandler(b.onGuildDelete)

	// ensure we know our application ID
	if b.Session.State.User == nil {
		if err := b.Session.Open(); err != nil {
			b.Logger.Error("failed to open session for command registration", "err", err)
			return
		}
	}

	appID := b.Session.State.User.ID

	//remove existing global commands incase there are any
	globalCommands, _ := b.Session.ApplicationCommands(appID, "")
	for _, cmd := range globalCommands {
		err := b.Session.ApplicationCommandDelete(appID, "", cmd.ID)
		if err != nil {
			b.Logger.Error("failed to delete existing global command", "name", cmd.Name, "id", cmd.ID, "err", err)
		} else {
			b.Logger.Debug("deleted existing global command", "name", cmd.Name, "id", cmd.ID)
		}
	}
}

// event handlers

// onInteractionUpdate handles all interaction events and routes them to the appropriate handler.
func (b *Bot) onInteractionUpdate(s *discordgo.Session, i *discordgo.InteractionCreate) {
	switch i.Type {
	case discordgo.InteractionMessageComponent:
		b.handleComponent(s, i)
	case discordgo.InteractionApplicationCommand:
		b.handleCommands(s, i)
	default:
		b.Logger.Warn("Unhandled interaction type", "type", i.Type)
	}
}

// onGuildCreate handles guild creation events.
func (b *Bot) onGuildCreate(s *discordgo.Session, g *discordgo.GuildCreate) {
	b.Logger.Info("joined guild", "name", g.Name, "id", g.ID)

	for name, def := range b.commands {
		if def.Command == nil {
			b.Logger.Warn("command has no definition", "name", name)
			continue
		}
		cmd, err := b.Session.ApplicationCommandCreate(s.State.User.ID, g.ID, def.Command)
		if err != nil {
			b.Logger.Error("failed to register command", "name", name, "guild_id", g.ID, "err", err)
			continue
		}
		b.Logger.Debug("registered command", "name", name, "id", cmd.ID, "guild_id", g.ID)
	}

	// Try to restore previous session if it exists
	b.restoreGuildSession(g.ID)
}

// restoreGuildSession attempts to restore a guild's previous streaming session
func (b *Bot) restoreGuildSession(guildID string) {
	savedState := b.sessionStore.Get(guildID)
	if savedState == nil {
		return // No saved state for this guild
	}

	station, ok := b.radioStations[savedState.StationID]
	if !ok {
		b.Logger.Warn("saved station no longer available", "guild_id", guildID, "station_id", savedState.StationID)
		// Clean up stale entry
		_ = b.sessionStore.Delete(guildID)
		return
	}

	b.Logger.Info("restoring previous session", "guild_id", guildID, "station", station.Name, "volume", savedState.Volume*100)

	// Create a restored session (but don't auto-join voice - let user start it)
	// Just set it up so when they use /radio again, it remembers their settings
	ctx, cancel := context.WithCancel(context.Background())
	b.radioMutex.Lock()
	b.radioSessions[guildID] = &StreamSession{
		Context: ctx,
		Cancel:  cancel,
		UserID:  "", // Will be set when user actually starts playback
		Station: &station,
		Volume:  savedState.Volume,
	}
	b.radioMutex.Unlock()
}

// onGuildDelete handles guild deletion events.
func (b *Bot) onGuildDelete(s *discordgo.Session, g *discordgo.GuildDelete) {
	b.Logger.Debug("left guild", "name", g.Name, "id", g.ID)
}

// onVoiceStateUpdate handles voice state updates to stop radio streaming when the bot leaves a voice channel.
func (bot *Bot) onVoiceStateUpdate(s *discordgo.Session, vs *discordgo.VoiceStateUpdate) {
	if vs.UserID != s.State.User.ID {
		return
	}

	// Check if the bot has disconnected from the voice channel
	if vs.ChannelID == "" {
		vc, hasVoiceConn := s.VoiceConnections[vs.GuildID]
		if !hasVoiceConn || vc == nil {
			guildID := vs.GuildID
			bot.Logger.Info("bot confirmed disconnected from voice", "guild_id", guildID)

			bot.radioMutex.Lock()
			if session, ok := bot.radioSessions[guildID]; ok && session != nil {
				bot.Logger.Debug("cancelling active radio session", "guild_id", guildID)
				session.Cancel()
				delete(bot.radioSessions, guildID)
			}
			bot.radioMutex.Unlock()
		}
	}
}
