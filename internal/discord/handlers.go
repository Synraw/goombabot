package discord

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/synraw/goombabot/internal/azurecast"
)

const (
	shortDelay          = 5 * time.Second        // 5 seconds
	mediumDelay         = 15 * time.Second       // 15 seconds
	longDelay           = 30 * time.Second       // 30 seconds
	maxLabelLen         = 100                    // Discord select option label limit
	DefaultVolume       = 1.0                    // 100%
	VolumeMin           = 5                      // 5%
	VolumeMax           = 500                    // 500%
	maxSelectOptions    = 25                     // Discord limit
	voiceDisconnectWait = 500 * time.Millisecond // Wait after disconnecting
)

// truncateLabel ensures select option labels meet Discord's 1–100 char limit.
func truncateLabel(label string) string {
	r := []rune(label)
	if len(r) <= maxLabelLen {
		return label
	}
	return string(r[:maxLabelLen-1]) + "…"
}

// ===== Response Helpers =====

// deleteMessageAfter deletes the interaction response after the given delay.
func deleteMessageAfter(s *discordgo.Session, i *discordgo.InteractionCreate, delay time.Duration) {
	go func() {
		time.Sleep(delay)
		_ = s.InteractionResponseDelete(i.Interaction)
	}()
}

// createResponse builds a simple text response.
func createResponse(content string) *discordgo.InteractionResponse {
	return &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: content,
		},
	}
}

// createResponseEx builds a response with custom data.
func createResponseEx(i *discordgo.InteractionResponseData) *discordgo.InteractionResponse {
	return &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: i,
	}
}

// ResponseBuilder provides a fluent interface for sending Discord responses.
type ResponseBuilder struct {
	session     *discordgo.Session
	interaction *discordgo.InteractionCreate
	bot         *Bot
}

// NewResponseBuilder creates a new response builder for an interaction.
func (bot *Bot) NewResponseBuilder(s *discordgo.Session, i *discordgo.InteractionCreate) *ResponseBuilder {
	return &ResponseBuilder{
		session:     s,
		interaction: i,
		bot:         bot,
	}
}

// Success sends a success message and deletes it after delay.
func (rb *ResponseBuilder) Success(msg string, delay time.Duration) {
	if err := rb.session.InteractionRespond(rb.interaction.Interaction, createResponse(msg)); err != nil {
		if rb.bot.Logger != nil {
			rb.bot.Logger.Error("failed to send success response", "err", err)
		}
		return
	}
	deleteMessageAfter(rb.session, rb.interaction, delay)
}

// Error logs and sends an error message, deleting after delay.
func (rb *ResponseBuilder) Error(msg string, err error, delay time.Duration) {
	if rb.bot.Logger != nil && err != nil {
		rb.bot.Logger.Warn(msg, "err", err)
	}
	if respondErr := rb.session.InteractionRespond(rb.interaction.Interaction, createResponse(msg)); respondErr != nil {
		if rb.bot.Logger != nil {
			rb.bot.Logger.Error("failed to send error response", "err", respondErr)
		}
		return
	}
	deleteMessageAfter(rb.session, rb.interaction, delay)
}

// WithComponents sends a response with components (buttons, select menus).
func (rb *ResponseBuilder) WithComponents(msg string, components ...discordgo.MessageComponent) {
	resp := &discordgo.InteractionResponseData{
		Content:    msg,
		Components: components,
		Flags:      discordgo.MessageFlagsEphemeral,
	}
	if err := rb.session.InteractionRespond(rb.interaction.Interaction, createResponseEx(resp)); err != nil {
		rb.bot.Logger.Warn("failed to send response with components", "err", err)
	}
}

// ===== Validation Helpers =====

// validateVoiceContextBasic validates basic voice channel connectivity without requiring an active session.
func (bot *Bot) validateVoiceContextBasic(s *discordgo.Session, i *discordgo.InteractionCreate, ignoreUserConnection bool) (*discordgo.Guild, error) {
	guild, err := s.State.Guild(i.GuildID)
	if err != nil {
		return nil, errors.New("could not get guild")
	}

	voiceChannelID := getUserVoiceChannelID(guild, i.Member.User.ID)
	if voiceChannelID == "" && !ignoreUserConnection {
		return nil, errors.New("you must be in a voice channel")
	}

	if s.VoiceConnections[guild.ID] == nil {
		return nil, errors.New("not connected to a voice channel")
	}

	return guild, nil
}

func (bot *Bot) validatePreJoinVoice(s *discordgo.Session, i *discordgo.InteractionCreate) (*discordgo.Guild, string, error) {
	guild, err := s.State.Guild(i.GuildID)
	if err != nil {
		return nil, "", errors.New("could not get guild")
	}
	voiceChannelID := getUserVoiceChannelID(guild, i.Member.User.ID)
	if voiceChannelID == "" {
		return nil, "", errors.New("you must be in a voice channel")
	}
	return guild, voiceChannelID, nil
}

func (bot *Bot) cleanupStaleVoiceState(s *discordgo.Session, guildID string) {
	session := bot.getStreamSession(guildID)
	vc := s.VoiceConnections[guildID]

	if session != nil && (vc == nil || !vc.Ready) {
		bot.Logger.Warn("clearing stale stream session; no ready voice connection", "guild_id", guildID)
		bot.streamMutex.Lock()
		delete(bot.streamSessions, guildID)
		bot.streamMutex.Unlock()
	}

	if session == nil && vc != nil {
		bot.Logger.Warn("disconnecting stale voice connection without session", "guild_id", guildID, "vc_ready", vc.Ready)
		_ = vc.Disconnect()
		time.Sleep(voiceDisconnectWait)
	}
}

// ===== Option Parsing Helpers =====

// getIntOption extracts an int64 from options at the given index.
func getIntOption(opts []*discordgo.ApplicationCommandInteractionDataOption, index int) (int64, error) {
	if len(opts) <= index {
		return 0, errors.New("option not found")
	}
	return opts[index].IntValue(), nil
}

// getStringOption extracts a string from options at the given index.
func getStringOption(opts []*discordgo.ApplicationCommandInteractionDataOption, index int) (string, error) {
	if len(opts) <= index {
		return "", errors.New("option not found")
	}
	return opts[index].StringValue(), nil
}

// ===== Voice Channel Helpers =====

// getUserVoiceChannelID returns the voice channel ID the user is currently in, or "" if not found.
func getUserVoiceChannelID(guild *discordgo.Guild, userID string) string {
	for _, vs := range guild.VoiceStates {
		if vs.UserID == userID {
			return vs.ChannelID
		}
	}
	return ""
}

// ===== UI Builders =====

// buildStationSelectMenu creates a select menu for choosing radio stations.
func (bot *Bot) buildStationSelectMenu() discordgo.SelectMenu {
	minValues := 1
	maxValues := 1
	menu := discordgo.SelectMenu{
		CustomID:    "radio_station_select",
		Placeholder: "Select a station",
		MinValues:   &minValues,
		MaxValues:   maxValues,
		Options:     []discordgo.SelectMenuOption{},
	}

	for id, station := range bot.radioStations {
		menu.Options = append(menu.Options, discordgo.SelectMenuOption{
			Label: truncateLabel(station.Name),
			Value: strconv.Itoa(id),
		})
	}

	return menu
}

func buildSongSelectMenu(songs map[string]azurecast.StationSongRequest) discordgo.SelectMenu {
	minValues := 1
	menu := discordgo.SelectMenu{
		CustomID:    "song_request_select",
		Placeholder: "Select a song to request",
		MinValues:   &minValues,
		MaxValues:   1,
		Options:     []discordgo.SelectMenuOption{},
	}

	count := 0
	for requestID, song := range songs {
		if count >= maxSelectOptions {
			break
		}
		menu.Options = append(menu.Options, discordgo.SelectMenuOption{
			Label: truncateLabel(song.Song.Title + " by " + song.Song.Artist),
			Value: requestID,
		})
		count++
	}

	return menu
}

// formatNowPlaying formats now playing information into a Discord message.
func formatNowPlaying(np *azurecast.NowPlaying) string {
	msg := "Now Playing:\n"

	title := np.NowPlaying.Song.Title
	if title == "" {
		title = "Unknown Title"
	}
	msg += "**" + title + "**"

	if np.NowPlaying.Song.Artist != "" {
		msg += " by **" + np.NowPlaying.Song.Artist + "**"
	} else {
		msg += " by Unknown Artist"
	}

	if np.NowPlaying.Song.Album != "" {
		msg += " from the album **" + np.NowPlaying.Song.Album + "**"
	}

	return msg
}

// ===== Song Request Logic =====

// requestSong handles the process of requesting a song on the radio station.
func (bot *Bot) requestSong(s *discordgo.Session, i *discordgo.InteractionCreate, song azurecast.StationSongRequest, stationID int) {
	response, err := bot.azureApiClient.RequestStationSong(context.Background(), strconv.Itoa(stationID), song)
	if err != nil {
		bot.NewResponseBuilder(s, i).Error("Failed to request the song.", err, shortDelay)
		return
	}

	if !response.Success {
		bot.NewResponseBuilder(s, i).Error("Failed to request the song: "+response.Message, nil, shortDelay)
		return
	}

	msg := "Requested song **" + song.Song.Title + "** by **" + song.Song.Artist + "** from the album **" + song.Song.Album + "**."
	bot.NewResponseBuilder(s, i).Success(msg, mediumDelay)
}

// findMatchingSongs searches for songs matching a query (case-insensitive).
func findMatchingSongs(songs []azurecast.StationSongRequest, query string) map[string]azurecast.StationSongRequest {
	results := make(map[string]azurecast.StationSongRequest)
	queryLower := strings.ToLower(query)

	for _, song := range songs {
		if strings.Contains(strings.ToLower(song.Song.Text), queryLower) {
			results[song.RequestID] = song
		}
	}

	return results
}

// findSongByID searches for a song by its request ID.
func findSongByID(songs []azurecast.StationSongRequest, requestID string) *azurecast.StationSongRequest {
	for i := range songs {
		if songs[i].RequestID == requestID {
			return &songs[i]
		}
	}
	return nil
}

// clampVolume ensures volume is within valid bounds
func clampVolume(v float64) float64 {
	min := float64(VolumeMin) / 100.0
	max := float64(VolumeMax) / 100.0
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

// ===== Radio Stream Logic =====

// runRadioStream handles the process of joining voice and streaming the radio.
func (bot *Bot) runRadioStream(s *discordgo.Session, i *discordgo.InteractionCreate, station RadioStation) {
	guild, voiceChannelID, err := bot.validatePreJoinVoice(s, i)
	if err != nil {
		bot.NewResponseBuilder(s, i).Error(err.Error(), nil, shortDelay)
		return
	}

	if station.StreamURL == "" {
		bot.NewResponseBuilder(s, i).Error("Selected station has no stream URL.", nil, shortDelay)
		return
	}

	// Check if already streaming
	bot.streamMutex.Lock()
	if existing := bot.streamSessions[guild.ID]; existing != nil {
		bot.streamMutex.Unlock()
		bot.NewResponseBuilder(s, i).Success("Already streaming. Use /stop first.", shortDelay)
		return
	}
	bot.streamMutex.Unlock()

	bot.cleanupStaleVoiceState(s, guild.ID)

	// Get volume from saved session or use default
	volume := DefaultVolume
	if saved := bot.sessionStore.Get(guild.ID); saved != nil && saved.StationID == station.ID {
		volume = clampVolume(saved.Volume)
	}

	// Create radio source
	radioSource := NewRadioSource(&station, guild.ID, bot.Logger)

	// Start streaming
	if err := bot.startStream(guild.ID, voiceChannelID, i.Member.User.ID, radioSource, volume); err != nil {
		bot.NewResponseBuilder(s, i).Error("Failed to start radio stream: "+err.Error(), nil, shortDelay)
		return
	}

	// Save session state
	if err := bot.sessionStore.Set(guild.ID, station.ID, volume); err != nil {
		bot.Logger.Warn("failed to persist session state", "guild_id", guild.ID, "err", err)
	}

	bot.Logger.Info("started streaming from station", "url", station.StreamURL, "name", station.Name, "guild", guild.Name)
	bot.NewResponseBuilder(s, i).Success("Starting radio: **"+station.Name+"**", shortDelay)
}

// ===== Command Handlers =====

// handleRadio initiates the radio streaming process.
func (bot *Bot) handleRadio(s *discordgo.Session, i *discordgo.InteractionCreate) {
	// Check if already streaming using new system
	session := bot.getStreamSession(i.GuildID)
	vc := s.VoiceConnections[i.GuildID]

	if session != nil && vc != nil && vc.Ready {
		bot.NewResponseBuilder(s, i).Success("Already streaming in a voice channel. Use /stop to stop the current stream first.", shortDelay)
		return
	}

	bot.cleanupStaleVoiceState(s, i.GuildID)

	if len(bot.radioStations) == 1 {
		for _, station := range bot.radioStations {
			bot.runRadioStream(s, i, station)
		}
		return
	}

	stationMenu := bot.buildStationSelectMenu()
	bot.NewResponseBuilder(s, i).WithComponents("Select a radio station:",
		discordgo.ActionsRow{
			Components: []discordgo.MessageComponent{stationMenu},
		},
	)
}

// handleStop stops the current stream and disconnects from voice.
func (bot *Bot) handleStop(s *discordgo.Session, i *discordgo.InteractionCreate) {
	guild, err := bot.validateVoiceContextBasic(s, i, true)
	if err != nil {
		bot.NewResponseBuilder(s, i).Error(err.Error(), nil, shortDelay)
		return
	}

	// Stop new streaming system
	if err := bot.stopStream(guild.ID); err != nil {
		bot.NewResponseBuilder(s, i).Error("No active stream to stop.", nil, shortDelay)
		return
	}

	voiceConn := s.VoiceConnections[guild.ID]
	if voiceConn != nil {
		_ = voiceConn.Disconnect()
	}

	bot.NewResponseBuilder(s, i).Success("Stopped the stream.", shortDelay)
}

// handleSkip skips the currently playing song on the radio station or music stream.
func (bot *Bot) handleSkip(s *discordgo.Session, i *discordgo.InteractionCreate) {
	// Check if there's an active stream
	session := bot.getStreamSession(i.GuildID)
	if session == nil {
		bot.NewResponseBuilder(s, i).Error("No active stream.", nil, shortDelay)
		return
	}

	// Only allow the user who started the stream to skip
	if session.UserID != i.Member.User.ID {
		bot.NewResponseBuilder(s, i).Error("Only the user who started the stream can skip.", nil, shortDelay)
		return
	}

	sourceType := session.Source.GetMetadata().Type

	// Handle radio stream skip
	if sourceType == "radio" {
		// Get station ID from session store
		savedState := bot.sessionStore.Get(i.GuildID)
		if savedState == nil {
			bot.NewResponseBuilder(s, i).Error("Could not find station information.", nil, shortDelay)
			return
		}

		err := bot.azureApiClient.SkipCurrentSong(context.Background(), strconv.Itoa(savedState.StationID))
		if err != nil {
			bot.NewResponseBuilder(s, i).Error("Failed to skip the current song.", err, shortDelay)
			return
		}

		bot.NewResponseBuilder(s, i).Success("Skipped the current song.", shortDelay)
		return
	}

	// Handle music stream skip (youtube, soundcloud, etc.)
	queue := bot.getMusicQueue(i.GuildID)
	nextSource := queue.Next()
	if nextSource == nil {
		bot.NewResponseBuilder(s, i).Error("No more songs in the queue.", nil, shortDelay)
		// Cancel the current stream to stop playback
		session.Cancel()
		return
	}

	// Update the stream session to play the next source
	session.Source = nextSource
	// Cancel the current context to trigger the next song
	session.Cancel()

	metadata := nextSource.GetMetadata()
	bot.NewResponseBuilder(s, i).Success(fmt.Sprintf("Skipped to: **%s** by %s", metadata.Title, metadata.Artist), shortDelay)
}

// handleNowPlaying shows the currently playing song on the radio station.
func (bot *Bot) handleNowPlaying(s *discordgo.Session, i *discordgo.InteractionCreate) {
	// Check if there's an active stream
	session := bot.getStreamSession(i.GuildID)
	if session == nil {
		bot.NewResponseBuilder(s, i).Error("No active stream.", nil, shortDelay)
		return
	}

	// Only works for radio streams
	if session.Source.GetMetadata().Type == "radio" {
		// Get station ID from session store
		savedState := bot.sessionStore.Get(i.GuildID)
		if savedState == nil {
			bot.NewResponseBuilder(s, i).Error("Could not find station information.", nil, shortDelay)
			return
		}

		np, err := bot.azureApiClient.GetStationNowPlaying(context.Background(), strconv.Itoa(savedState.StationID))
		if err != nil {
			bot.NewResponseBuilder(s, i).Error("Failed to get now playing information.", err, shortDelay)
			return
		}

		bot.NewResponseBuilder(s, i).Success(formatNowPlaying(np), longDelay)
		return
	}

	bot.NewResponseBuilder(s, i).Success("Now Playing **"+session.Source.GetMetadata().Title+"** by **"+session.Source.GetMetadata().Artist+"**", longDelay)
}

// handleRequest handles song requests for the radio station.
func (bot *Bot) handleRequest(s *discordgo.Session, i *discordgo.InteractionCreate) {
	// Check if there's an active stream
	session := bot.getStreamSession(i.GuildID)
	if session == nil {
		bot.NewResponseBuilder(s, i).Error("No active stream.", nil, shortDelay)
		return
	}

	// Only works for radio streams
	if session.Source.GetMetadata().Type != "radio" {
		bot.NewResponseBuilder(s, i).Error("Song request only works with radio streams.", nil, shortDelay)
		return
	}

	// Get station ID from session store
	savedState := bot.sessionStore.Get(i.GuildID)
	if savedState == nil {
		bot.NewResponseBuilder(s, i).Error("Could not find station information.", nil, shortDelay)
		return
	}

	opts := i.ApplicationCommandData().Options
	songName, err := getStringOption(opts, 0)
	if err != nil {
		bot.NewResponseBuilder(s, i).Error("Song name is required.", err, shortDelay)
		return
	}

	requestableSongs, err := bot.azureApiClient.GetStationRequestableSongs(context.Background(), strconv.Itoa(savedState.StationID))
	if err != nil {
		bot.NewResponseBuilder(s, i).Error("Failed to get requestable songs.", err, shortDelay)
		return
	}

	matchingSongs := findMatchingSongs(requestableSongs, songName)
	if len(matchingSongs) == 0 {
		bot.NewResponseBuilder(s, i).Error("Song not found in requestable list.", nil, shortDelay)
		return
	}

	if len(matchingSongs) == 1 {
		for _, song := range matchingSongs {
			bot.requestSong(s, i, song, savedState.StationID)
		}
		return
	}

	songMenu := buildSongSelectMenu(matchingSongs)
	bot.NewResponseBuilder(s, i).WithComponents("Multiple songs found. Please select one (only showing the first "+strconv.Itoa(maxSelectOptions)+"):",
		discordgo.ActionsRow{
			Components: []discordgo.MessageComponent{songMenu},
		},
	)
}

// handleVolume adjusts the streaming volume for the current session.
func (bot *Bot) handleVolume(s *discordgo.Session, i *discordgo.InteractionCreate) {
	// Get current stream session
	session := bot.getStreamSession(i.GuildID)
	if session == nil {
		bot.NewResponseBuilder(s, i).Error("No active stream.", nil, shortDelay)
		return
	}

	opts := i.ApplicationCommandData().Options
	volumeVal, err := getIntOption(opts, 0)
	if err != nil {
		bot.NewResponseBuilder(s, i).Error("Volume value is required.", err, shortDelay)
		return
	}

	if volumeVal < VolumeMin || volumeVal > VolumeMax {
		bot.NewResponseBuilder(s, i).Error("Volume must be between "+strconv.Itoa(VolumeMin)+" and "+strconv.Itoa(VolumeMax)+".", nil, shortDelay)
		return
	}

	bot.streamMutex.Lock()
	oldVolume := int(session.Volume * 100)
	session.Volume = float64(volumeVal) / 100.0
	bot.streamMutex.Unlock()

	// If it's a radio stream, persist the volume change
	if session.Source.GetMetadata().Type == "radio" {
		// Try to get station ID from saved state
		if saved := bot.sessionStore.Get(i.GuildID); saved != nil {
			if err := bot.sessionStore.Set(i.GuildID, saved.StationID, float64(volumeVal)/100.0); err != nil {
				bot.Logger.Warn("failed to persist volume change", "guild_id", i.GuildID, "err", err)
			}
		}
	}

	msg := "Set volume from " + strconv.Itoa(oldVolume) + "% to " + strconv.FormatInt(int64(volumeVal), 10) + "%"
	bot.NewResponseBuilder(s, i).Success(msg, shortDelay)
}

// ===== Component Handlers =====

// handleComponent routes interaction component events to the appropriate handler.
func (bot *Bot) handleComponent(s *discordgo.Session, i *discordgo.InteractionCreate) {
	switch i.MessageComponentData().CustomID {
	case "radio_station_select":
		bot.handleRadioSelect(s, i)
	case "song_request_select":
		bot.handleSongRequestSelect(s, i)
	}
}

// handleRadioSelect processes the station selection and starts streaming.
func (bot *Bot) handleRadioSelect(s *discordgo.Session, i *discordgo.InteractionCreate) {
	values := i.MessageComponentData().Values
	if len(values) == 0 {
		bot.NewResponseBuilder(s, i).Error("No station selected.", nil, shortDelay)
		return
	}

	stationID, err := strconv.Atoi(values[0])
	if err != nil {
		bot.NewResponseBuilder(s, i).Error("Invalid station ID.", err, shortDelay)
		return
	}

	station, ok := bot.radioStations[stationID]
	if !ok {
		bot.NewResponseBuilder(s, i).Error("Station not found.", nil, shortDelay)
		return
	}

	bot.runRadioStream(s, i, station)
}

// handleSongRequestSelect processes the song selection and requests it.
func (bot *Bot) handleSongRequestSelect(s *discordgo.Session, i *discordgo.InteractionCreate) {
	values := i.MessageComponentData().Values
	if len(values) == 0 {
		bot.NewResponseBuilder(s, i).Success("No song selected.", shortDelay)
		return
	}

	// Check if there's an active stream
	session := bot.getStreamSession(i.GuildID)
	if session == nil {
		bot.NewResponseBuilder(s, i).Error("No active stream.", nil, shortDelay)
		return
	}

	// Get station ID from session store
	savedState := bot.sessionStore.Get(i.GuildID)
	if savedState == nil {
		bot.NewResponseBuilder(s, i).Error("Could not find station information.", nil, shortDelay)
		return
	}

	requestID := values[0]

	requestableSongs, err := bot.azureApiClient.GetStationRequestableSongs(context.Background(), strconv.Itoa(savedState.StationID))
	if err != nil {
		bot.NewResponseBuilder(s, i).Error("Failed to get requestable songs.", err, shortDelay)
		return
	}

	selectedSong := findSongByID(requestableSongs, requestID)
	if selectedSong == nil {
		bot.NewResponseBuilder(s, i).Error("Selected song not found.", nil, shortDelay)
		return
	}

	bot.requestSong(s, i, *selectedSong, savedState.StationID)
}

// ===== Command Router =====

// handleCommands routes interaction command events to the appropriate handler.
func (bot *Bot) handleCommands(s *discordgo.Session, i *discordgo.InteractionCreate) {
	data := i.ApplicationCommandData()
	def, ok := bot.commands[data.Name]
	if !ok || def.Handle == nil {
		bot.Logger.Warn("no handler for command", "name", data.Name)
		return
	}
	def.Handle(bot, s, i)
}

// ===== Music Commands =====

// handlePlay handles the /play command to play music from URLs
func (bot *Bot) handlePlay(s *discordgo.Session, i *discordgo.InteractionCreate) {
	// Validate voice context
	guild, voiceChannelID, err := bot.validatePreJoinVoice(s, i)
	if err != nil {
		bot.NewResponseBuilder(s, i).Error(err.Error(), nil, shortDelay)
		return
	}

	// Get URL from command options
	opts := i.ApplicationCommandData().Options
	if len(opts) == 0 {
		bot.NewResponseBuilder(s, i).Error("No URL provided.", nil, shortDelay)
		return
	}
	input := opts[0].StringValue()

	// Defer response so we can take time to process
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	}); err != nil {
		bot.Logger.Error("failed to defer response", "err", err)
		return
	}

	// If input is not a URL, treat it as a YouTube search query
	url := input
	if !strings.Contains(input, "://") {
		url = "ytsearch1:" + input
	}

	// Create music source
	musicSource, err := NewMusicSource(url, guild.ID, bot.Logger)
	if err != nil {
		_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
			Content: strPtr("Failed to load music: " + err.Error()),
		})
		deleteMessageAfter(s, i, mediumDelay)
		return
	}

	metadata := musicSource.GetMetadata()
	bot.Logger.Debug("loaded music", "guild_id", guild.ID, "title", metadata.Title, "type", metadata.Type)

	// Check if there's already an active stream
	currentSession := bot.getStreamSession(guild.ID)
	if currentSession != nil {
		// Add to queue
		queue := bot.getMusicQueue(guild.ID)
		queue.Add(musicSource)

		_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
			Content: strPtr(fmt.Sprintf("Added to queue: **%s** by %s (Position: %d)", metadata.Title, metadata.Artist, queue.Size())),
		})
		deleteMessageAfter(s, i, mediumDelay)
		return
	}

	// No active stream, start playing immediately
	volume := DefaultVolume
	if err := bot.startStream(guild.ID, voiceChannelID, i.Member.User.ID, musicSource, volume); err != nil {
		_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
			Content: strPtr("Failed to start playback: " + err.Error()),
		})
		deleteMessageAfter(s, i, mediumDelay)
		return
	}

	_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Content: strPtr(fmt.Sprintf("Now playing: **%s** by %s", metadata.Title, metadata.Artist)),
	})
	deleteMessageAfter(s, i, mediumDelay)
}

// handleQueue handles the /queue command to show the music queue
func (bot *Bot) handleQueue(s *discordgo.Session, i *discordgo.InteractionCreate) {
	queue := bot.getMusicQueue(i.GuildID)

	if queue.IsEmpty() {
		bot.NewResponseBuilder(s, i).Success("The queue is empty.", shortDelay)
		return
	}

	// Build queue message
	msg := "**Music Queue:**\n"
	msg += fmt.Sprint("Currently playing: **", queue.Current().GetMetadata().Title, "** by ", queue.Current().GetMetadata().Artist, "\n\n")
	items := queue.List()
	for idx, source := range items {
		metadata := source.GetMetadata()
		msg += fmt.Sprintf("%d. **%s** by %s", idx+1, metadata.Title, metadata.Artist)

		if metadata.Duration > 0 {
			msg += fmt.Sprintf(" (%s)", formatDuration(metadata.Duration))
		}
		msg += "\n"
	}

	bot.NewResponseBuilder(s, i).Success(msg, longDelay)
}

// Helper function to convert string pointer
func strPtr(s string) *string {
	return &s
}

// Helper function to format duration
func formatDuration(d time.Duration) string {
	hours := int(d.Hours())
	minutes := int(d.Minutes()) % 60
	seconds := int(d.Seconds()) % 60

	if hours > 0 {
		return fmt.Sprintf("%d:%02d:%02d", hours, minutes, seconds)
	}
	return fmt.Sprintf("%d:%02d", minutes, seconds)
}
