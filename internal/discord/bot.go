package discord

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
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
}

type Bot struct {
	Token           string
	Session         *discordgo.Session
	Logger          *slog.Logger
	commands        map[string]CommandDef
	config          *config.Config
	azurecastClient *azurecast.Client
	radioStations   map[int]RadioStation
	mutex           sync.Mutex
	radioCancel     map[string]chan struct{}
}

// New creates a new Discord bot instance.
func New(token string, logger *slog.Logger, cfg *config.Config) (*Bot, error) {
	sess, err := discordgo.New("Bot " + token)
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
		radioCancel:   make(map[string]chan struct{}),
	}

	// Initialize Azurecast client
	bot.azurecastClient, err = azurecast.NewClient(cfg.AzurecastApiUrl,
		azurecast.WithAPIKey(cfg.AzurecastToken),
	)

	if err != nil {
		return nil, err
	}

	radioStations, _ := bot.azurecastClient.GetStations(context.Background())

	for _, station := range radioStations {
		opusMountUrl := station.ListenURL
		for _, mount := range station.Mounts {
			if mount.Format == "opus" {
				opusMountUrl = mount.URL
			}
		}
		bot.Logger.Info("Found station playing", "name", station.Name, "id", station.ID)
		bot.radioStations[station.ID] = RadioStation{
			ID:        station.ID,
			Name:      station.Name,
			StreamURL: opusMountUrl,
		}
	}

	bot.AddCommand("radio", "Play a station from your Azurecast server in the current voice channel", (*Bot).handleRadio)
	bot.AddCommand("stop", "Stops the currently streaming radio from playing", (*Bot).handleStop)

	return bot, nil
}

// Start opens the Discord session and begins listening for events.
func (b *Bot) Start(ctx context.Context) error {
	if err := b.Session.Open(); err != nil {
		return err
	}

	b.RegisterCommands()
	b.Logger.Info("Discord bot started")
	defer b.Session.Close()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-ctx.Done():
	case <-stop:
		b.Logger.Info("Shutting down Discord bot")
	}
	return nil
}

// NewCommandDef is the *only* place you construct a CommandDef.
func NewCommandDef(
	name, description string,
	handler CommandHandler,
	opts ...func(*discordgo.ApplicationCommand),
) CommandDef {
	cmd := &discordgo.ApplicationCommand{
		Name:        name,
		Description: description,
		Type:        discordgo.ChatApplicationCommand,
	}
	for _, opt := range opts {
		opt(cmd)
	}
	return CommandDef{
		Command: cmd,
		Handle:  handler,
	}
}

// AddCommand adds a command definition to the bot.
func (b *Bot) AddCommand(command string, description string, handler CommandHandler, opts ...func(*discordgo.ApplicationCommand)) {
	b.commands[command] = NewCommandDef(command, description, handler, opts...)
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
		b.Logger.Info("registered command", "name", name, "id", cmd.ID, "guild_id", g.ID)
	}
}

// onGuildDelete handles guild deletion events.
func (b *Bot) onGuildDelete(s *discordgo.Session, g *discordgo.GuildDelete) {
	b.Logger.Info("left guild", "name", g.Name, "id", g.ID)
}

// onInteractionUpdate handles all interaction events and routes them to the appropriate handler.
func (b *Bot) onInteractionUpdate(s *discordgo.Session, i *discordgo.InteractionCreate) {
	switch i.Type {
	case discordgo.InteractionMessageComponent:
		b.handleComponent(s, i)
	case discordgo.InteractionApplicationCommand:
		data := i.ApplicationCommandData()
		def, ok := b.commands[data.Name]
		if !ok || def.Handle == nil {
			b.Logger.Warn("no handler for command", "name", data.Name)
			return
		}
		def.Handle(b, s, i)
	default:
		b.Logger.Warn("unknown interaction type", "type", i.Type)
	}
}

// onVoiceStateUpdate handles voice state updates to stop radio streaming when the bot leaves a voice channel.
func (bot *Bot) onVoiceStateUpdate(s *discordgo.Session, vs *discordgo.VoiceStateUpdate) {
	if vs.UserID != s.State.User.ID {
		return
	}
	if vs.ChannelID == "" { // Bot has disconnected from voice channel
		guildID := vs.GuildID
		bot.mutex.Lock()
		if cancel, ok := bot.radioCancel[guildID]; ok {
			close(cancel) // Signal the stream to stop
			delete(bot.radioCancel, guildID)
		}
		bot.mutex.Unlock()
		if vc, ok := s.VoiceConnections[guildID]; ok {
			_ = vc.Disconnect()
		}
	}
}
