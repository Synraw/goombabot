package discord

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/synraw/goombabot/internal/azurecast"
	"github.com/synraw/goombabot/internal/config"
)

// CommandHandler defines the function signature for command handlers.
type CommandHandler func(b *Bot, s *discordgo.Session, i *discordgo.InteractionCreate)

// CommandDef represents a command definition with its handler.
type CommandDef struct {
	Command *discordgo.ApplicationCommand
	Handle  CommandHandler
}

// RadioStation represents a radio station fetched from Azurecast
type RadioStation struct {
	ID        int    // unique station ID
	Name      string // station name
	StreamURL string // URL to stream the station
	IsOpus    bool   // whether the stream is in Opus format
}

// Bot represents the Discord bot instance
type Bot struct {
	Token          string                    // Discord bot token
	Session        *discordgo.Session        // Discord session
	Logger         *slog.Logger              // structured logger
	commands       map[string]CommandDef     // registered commands
	config         *config.Config            // bot configuration
	azureApiClient *azurecast.Client         // Azurecast API client
	radioStations  map[int]RadioStation      // available radio stations
	streamMutex    sync.Mutex                // mutex for stream session access
	streamSessions map[string]*StreamSession // active stream sessions by guild ID
	sessionStore   *SessionStore             // persistent session store
	voiceStreamer  *VoiceStreamer            // voice streamer for generic audio sources
	musicQueues    map[string]*MusicQueue    // music queues by guild ID
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
		Token:          token,
		Session:        sess,
		Logger:         logger,
		config:         cfg,
		commands:       map[string]CommandDef{},
		radioStations:  make(map[int]RadioStation),
		streamSessions: make(map[string]*StreamSession),
		sessionStore:   sessionStore,
		musicQueues:    make(map[string]*MusicQueue),
	}

	// Initialize voice streamer
	bot.voiceStreamer = NewVoiceStreamer(bot)

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

		bot.Logger.Debug("station found", "name", station.Name, "id", station.ID)
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
	bot.AddCommand("nowplaying", "Shows the currently playing song on the current media", (*Bot).handleNowPlaying)
	bot.AddCommand("play", "Play music from YouTube or other supported sources", (*Bot).handlePlay,
		&discordgo.ApplicationCommandOption{
			Type:        discordgo.ApplicationCommandOptionString,
			Name:        "query",
			Description: "URL or a search query to play music from",
			Required:    true,
		},
	)
	bot.AddCommand("queue", "Show the current music queue", (*Bot).handleQueue)
	bot.AddCommand("volume", "Set the volume for the current media", (*Bot).handleVolume,
		&discordgo.ApplicationCommandOption{
			Type:        discordgo.ApplicationCommandOptionInteger,
			Name:        "level",
			Description: "Volume level from " + strconv.Itoa(VolumeMin) + " to " + strconv.Itoa(VolumeMax) + " percent (default " + strconv.Itoa(int(DefaultVolume*100)) + "%)",
			Required:    true,
		},
	)
	bot.AddCommand("repeat", "Set or get the current music queue repeating mode (only valid for music streams, not radios)", (*Bot).handleRepeat,
		&discordgo.ApplicationCommandOption{
			Type:        discordgo.ApplicationCommandOptionString,
			Name:        "mode",
			Description: "Repeat mode: none, one, all",
			Required:    false,
			Choices: []*discordgo.ApplicationCommandOptionChoice{
				{
					Name:  "none",
					Value: "none",
				},
				{
					Name:  "one",
					Value: "one",
				},
				{
					Name:  "all",
					Value: "all",
				},
			},
		})
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
	b.Logger.Info("Discord bot started")
	defer b.Session.Close()

	<-ctx.Done()
	// Stop all active stream sessions
	b.streamMutex.Lock()
	for guildID, session := range b.streamSessions {
		b.Logger.Debug("stopping stream session", "guild_id", guildID)
		session.Cancel()
	}
	b.streamMutex.Unlock()
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

	// Register commands for this guild
	for name, def := range b.commands {
		if def.Command == nil {
			b.Logger.Warn("command has no definition", "name", name)
			continue
		}
		_, err := b.Session.ApplicationCommandCreate(s.State.User.ID, g.ID, def.Command)
		if err != nil {
			b.Logger.Error("failed to register command", "name", name, "guild_id", g.ID, "err", err)
			continue
		}
	}

	// Clean up old bot messages in all channels
	maxDuration := 10 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), maxDuration)
	defer cancel()

	for _, channel := range g.Channels {
		if channel.Type != discordgo.ChannelTypeGuildText {
			continue
		}

		// Check timeout before fetching messages
		select {
		case <-ctx.Done():
			b.Logger.Warn("cleanup operation timed out, stopping further processing", "max_duration", maxDuration)
			return
		default:
		}

		messages, err := s.ChannelMessages(channel.ID, 10, "", "", "")
		if err == nil {
			for _, message := range messages {
				if message.Author.ID == s.State.User.ID {
					b.Logger.Debug("deleting old bot message", "channel_id", channel.ID, "message_id", message.ID)
					if err := s.ChannelMessageDelete(channel.ID, message.ID); err != nil {
						status := 0
						if restErr, ok := err.(*discordgo.RESTError); ok && restErr.Response != nil {
							status = restErr.Response.StatusCode
						}
						b.Logger.Warn("failed to delete old bot message", "channel_id", channel.ID, "message_id", message.ID, "status", status, "err", err)
					}
				}
			}
		}
	}
}

// onGuildDelete handles guild deletion events.
func (b *Bot) onGuildDelete(s *discordgo.Session, g *discordgo.GuildDelete) {
	b.Logger.Debug("left guild", "name", g.Name, "id", g.ID)
}

// onVoiceStateUpdate handles voice state updates to stop streaming when the bot leaves a voice channel.
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

			// Stop stream session
			bot.streamMutex.Lock()
			if session, ok := bot.streamSessions[guildID]; ok && session != nil {
				bot.Logger.Debug("cancelling active stream session", "guild_id", guildID)
				session.Cancel()
				delete(bot.streamSessions, guildID)
			}
			bot.streamMutex.Unlock()
		}
	}
}

// startStream starts streaming audio from the given source to the voice channel
func (bot *Bot) startStream(guildID, channelID, voiceChannelID, userID string, source AudioSource, volume float64, repeatMode AudioRepeatType) error {
	// Join voice channel
	vc, err := bot.Session.ChannelVoiceJoin(guildID, voiceChannelID, false, true)
	if err != nil {
		return fmt.Errorf("failed to join voice channel: %w", err)
	}

	// Create stream session
	ctx, cancel := context.WithCancel(context.Background())
	session := &StreamSession{
		Context:    ctx,
		Cancel:     cancel,
		UserID:     userID,
		GuildID:    guildID,
		Volume:     volume,
		RepeatMode: repeatMode,
		Source:     source,
	}

	// Store session
	bot.streamMutex.Lock()
	bot.streamSessions[guildID] = session
	bot.streamMutex.Unlock()

	// Start streaming in a goroutine
	go func() {
		// Loop to play through queue items
		for {
			if err := bot.voiceStreamer.Stream(vc, session); err != nil {
				bot.Logger.Error("stream error", "guild_id", guildID, "err", err)
				break
			}

			// Check if there's a next song in the queue (for any non-radio source)
			sourceType := session.Source.GetMetadata().Type
			if sourceType != "radio" {
				if session.RepeatMode == AudioRepeatOne {
					// Repeat the same source
					ctx, cancel := context.WithCancel(context.Background())
					session.Context = ctx
					session.Cancel = cancel
					continue
				}
				queue := bot.getMusicQueue(guildID)
				nextSource := queue.Next(session.RepeatMode)
				if nextSource == nil {
					// Queue is empty, end streaming
					bot.Logger.Debug("queue finished, ending stream", "guild_id", guildID)
					break
				}

				// Update session to next source and create new context
				session.Source = nextSource
				ctx, cancel := context.WithCancel(context.Background())
				session.Context = ctx
				session.Cancel = cancel

				metadata := nextSource.GetMetadata()

				var strBuild strings.Builder

				fmt.Fprintf(&strBuild, " Now Playing: [**%s** by %s](<%s>)", metadata.Title, metadata.Artist, metadata.URL)

				if metadata.Duration > 0 {
					fmt.Fprintf(&strBuild, " (%s)", formatDuration(metadata.Duration))
				}

				if userID != "" {
					fmt.Fprintf(&strBuild, " - requested by <@%s>", userID)
				}

				msg, _ := bot.Session.ChannelMessageSend(channelID, strBuild.String())

				go func() {
					time.Sleep(longDelay)
					_ = bot.Session.ChannelMessageDelete(channelID, msg.ID)
				}()

				bot.Logger.Debug("playing next song from queue", "guild_id", guildID, "title", metadata.Title, "artist", metadata.Artist)

				continue
			}

			// Radio streams don't have queues, end after stream finishes
			break
		}

		// Clean up after streaming finishes
		bot.streamMutex.Lock()
		delete(bot.streamSessions, guildID)
		bot.streamMutex.Unlock()

		// Disconnect from voice
		if err := vc.Disconnect(); err != nil {
			bot.Logger.Warn("error disconnecting from voice", "guild_id", guildID, "err", err)
		}
	}()

	return nil
}

// stopStream stops any active stream in the guild
func (bot *Bot) stopStream(guildID string) error {
	bot.streamMutex.Lock()
	session, ok := bot.streamSessions[guildID]
	bot.streamMutex.Unlock()

	if !ok || session == nil {
		return fmt.Errorf("no active stream in this guild")
	}

	session.Cancel()
	return nil
}

// getStreamSession returns the current stream session for a guild
func (bot *Bot) getStreamSession(guildID string) *StreamSession {
	bot.streamMutex.Lock()
	defer bot.streamMutex.Unlock()
	return bot.streamSessions[guildID]
}

// getMusicQueue returns the music queue for a guild, creating one if needed
func (bot *Bot) getMusicQueue(guildID string) *MusicQueue {
	bot.streamMutex.Lock()
	defer bot.streamMutex.Unlock()

	if queue, ok := bot.musicQueues[guildID]; ok {
		return queue
	}

	queue := NewMusicQueue()
	bot.musicQueues[guildID] = queue
	return queue
}
