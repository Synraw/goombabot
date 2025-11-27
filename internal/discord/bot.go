package discord

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/bwmarrin/discordgo"
)

type CommandHandler func(s *discordgo.Session, i *discordgo.InteractionCreate)

type CommandDef struct {
	Command *discordgo.ApplicationCommand
	Handle  CommandHandler
}

type Bot struct {
	Token    string
	Session  *discordgo.Session
	Logger   *slog.Logger
	commands map[string]CommandDef
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

func New(token string, logger *slog.Logger) (*Bot, error) {
	sess, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, err
	}

	bot := &Bot{
		Token:   token,
		Session: sess,
		Logger:  logger,
		commands: map[string]CommandDef{
			// simple ping command
			"ping": NewCommandDef("ping", "Replies with Pong!", handlePing),

			// radio command (from your planning doc) – arguments TBD
			"radio": NewCommandDef(
				"radio",
				"Play a station from your Azurecast server in the current voice channel",
				handleRadio,
				// optional: add options here when you know them
				// WithStringOption("station", "Station to play", true),
			),
		},
	}

	return bot, nil
}

// Example "option" helper to configure ApplicationCommand
func WithStringOption(
	name, description string,
	required bool,
) func(*discordgo.ApplicationCommand) {
	return func(cmd *discordgo.ApplicationCommand) {
		cmd.Options = append(cmd.Options, &discordgo.ApplicationCommandOption{
			Type:        discordgo.ApplicationCommandOptionString,
			Name:        name,
			Description: description,
			Required:    required,
		})
	}
}

// RegisterCommands registers all commands with Discord.
func (b *Bot) RegisterCommands() {
	// route all interaction events to our dispatcher
	b.Session.AddHandler(b.commandHandler)

	// ensure we know our application ID
	if b.Session.State.User == nil {
		if err := b.Session.Open(); err != nil {
			b.Logger.Error("failed to open session for command registration", "err", err)
			return
		}
	}

	appID := b.Session.State.User.ID

	for name, def := range b.commands {
		if def.Command == nil {
			b.Logger.Warn("command has no definition", "name", name)
			continue
		}
		if _, err := b.Session.ApplicationCommandCreate(appID, "", def.Command); err != nil {
			b.Logger.Error("failed to register command", "name", name, "err", err)
		}
	}
}

func (b *Bot) commandHandler(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.Type != discordgo.InteractionApplicationCommand {
		return
	}

	data := i.ApplicationCommandData()
	def, ok := b.commands[data.Name]
	if !ok || def.Handle == nil {
		b.Logger.Warn("no handler for command", "name", data.Name)
		return
	}

	def.Handle(s, i)
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
