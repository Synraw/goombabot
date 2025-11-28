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
	ListenUrl string
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
		bot.Logger.Info("Found station playing", "name", station.Name, "id", station.ID)
		bot.radioStations[station.ID] = RadioStation{
			ID:        station.ID,
			Name:      station.Name,
			ListenUrl: station.Mounts[1].URL,
		}
	}

	bot.AddCommand("radio", "Play a station from your Azurecast server in the current voice channel", (*Bot).handleRadio)

	return bot, nil
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

func (b *Bot) AddCommand(command string, description string, handler CommandHandler, opts ...func(*discordgo.ApplicationCommand)) {
	b.commands[command] = NewCommandDef(command, description, handler, opts...)
}

// RegisterCommands registers all commands with Discord.
func (b *Bot) RegisterCommands() {
	// route all interaction events to our dispatcher
	b.Session.AddHandler(b.commandHandler)

	// route voice state updates to our handler
	b.Session.AddHandler(b.onVoiceStateUpdate)

	// ensure we know our application ID
	if b.Session.State.User == nil {
		if err := b.Session.Open(); err != nil {
			b.Logger.Error("failed to open session for command registration", "err", err)
			return
		}
	}

	appID := b.Session.State.User.ID

	//guildCommands, _ := b.Session.ApplicationCommands(appID, b.config.DiscordGuildID)
	// for _, cmd := range guildCommands {
	// 	err := b.Session.ApplicationCommandDelete(appID, b.config.DiscordGuildID, cmd.ID)
	// 	if err != nil {
	// 		b.Logger.Error("failed to delete existing guild command", "name", cmd.Name, "id", cmd.ID, "err", err)
	// 	} else {
	// 		b.Logger.Debug("deleted existing guild command", "name", cmd.Name, "id", cmd.ID)
	// 	}
	// }

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

	for name, def := range b.commands {
		if def.Command == nil {
			b.Logger.Warn("command has no definition", "name", name)
			continue
		}

		//TODO: itterate our joined guilds and register command for all of them
		cmd, err := b.Session.ApplicationCommandCreate(appID, b.config.DiscordGuildID, def.Command)
		if err != nil {
			b.Logger.Error("failed to register command", "name", name, "err", err)
			continue
		}

		b.Logger.Info("registered command", "name", name, "id", cmd.ID)
	}
}

// event handlers

func (b *Bot) commandHandler(s *discordgo.Session, i *discordgo.InteractionCreate) {
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

// Fired whenever someone's voice state changes.
func (bot *Bot) onVoiceStateUpdate(s *discordgo.Session, vs *discordgo.VoiceStateUpdate) {
	if vs.UserID != s.State.User.ID {
		return
	}
	if vs.ChannelID == "" {
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
