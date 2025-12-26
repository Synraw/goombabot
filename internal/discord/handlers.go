package discord

import (
	"context"
	"errors"
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

// VoiceContext holds validated voice channel state.
type VoiceContext struct {
	Guild        *discordgo.Guild
	VoiceChannel string
	Session      *StreamSession
}

// validateVoiceContext validates that the user is in a voice channel with an active stream.
func (bot *Bot) validateVoiceContext(s *discordgo.Session, i *discordgo.InteractionCreate, ignoreUserConnection bool) (*VoiceContext, error) {
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

	session, ok := bot.radioSessions[guild.ID]
	if !ok {
		return nil, errors.New("no active radio session")
	}

	return &VoiceContext{
		Guild:        guild,
		VoiceChannel: voiceChannelID,
		Session:      session,
	}, nil
}

// validateVoiceContextBasic validates basic voice channel connectivity without requiring an active session.
func (bot *Bot) validateVoiceContextBasic(s *discordgo.Session, i *discordgo.InteractionCreate, ignoreUserConnection bool) (*VoiceContext, error) {
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

	return &VoiceContext{
		Guild:        guild,
		VoiceChannel: voiceChannelID,
	}, nil
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
	bot.radioMutex.Lock()
	session := bot.radioSessions[guildID]
	bot.radioMutex.Unlock()

	vc := s.VoiceConnections[guildID]
	if session != nil && (vc == nil || !vc.Ready) {
		bot.Logger.Warn("clearing stale radio session; no ready voice connection", "guild_id", guildID)
		bot.radioMutex.Lock()
		delete(bot.radioSessions, guildID)
		bot.radioMutex.Unlock()
	}

	vc = s.VoiceConnections[guildID]
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

func (vc *VoiceContext) StationIDString() string {
	if vc.Session == nil || vc.Session.Station == nil {
		return ""
	}
	return strconv.Itoa(vc.Session.Station.ID)
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

	bot.radioMutex.Lock()
	if existing := bot.radioSessions[guild.ID]; existing != nil {
		if c := s.VoiceConnections[guild.ID]; c != nil && c.Ready {
			bot.radioMutex.Unlock()
			bot.NewResponseBuilder(s, i).Success("Already streaming. Use /stop first.", shortDelay)
			return
		}
		bot.Logger.Warn("stale radio session found; cleaning up", "guild_id", guild.ID)
		delete(bot.radioSessions, guild.ID)
	}
	bot.radioMutex.Unlock()

	bot.cleanupStaleVoiceState(s, guild.ID)

	bot.Logger.Debug("joining voice", "guild_id", guild.ID, "channel_id", voiceChannelID)
	vc, err := s.ChannelVoiceJoin(guild.ID, voiceChannelID, false, true)
	if err != nil {
		bot.NewResponseBuilder(s, i).Error("Failed to join voice channel.", err, shortDelay)
		return
	}
	bot.Logger.Debug("joined voice", "guild_id", guild.ID)

	volume := DefaultVolume
	if saved := bot.sessionStore.Get(guild.ID); saved != nil && saved.StationID == station.ID {
		volume = clampVolume(saved.Volume)
	}

	bot.radioMutex.Lock()
	if existing := bot.radioSessions[guild.ID]; existing != nil {
		bot.radioMutex.Unlock()
		bot.NewResponseBuilder(s, i).Success("Already streaming. Use /stop first.", shortDelay)
		_ = vc.Disconnect()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	session := &StreamSession{
		Context: ctx,
		Cancel:  cancel,
		GuildID: guild.ID,
		UserID:  i.Member.User.ID,
		Station: &station,
		Volume:  volume,
	}
	bot.radioSessions[guild.ID] = session
	bot.radioMutex.Unlock()

	if err := bot.sessionStore.Set(guild.ID, station.ID, volume); err != nil {
		bot.Logger.Warn("failed to persist session state", "guild_id", guild.ID, "err", err)
	}

	go func() {
		defer func() {
			bot.Logger.Info("stopped streaming from station", "name", station.Name, "guild", guild.Name)

			// Ensure proper disconnect with error logging
			if err := vc.Disconnect(); err != nil {
				bot.Logger.Warn("error disconnecting voice", "guild_id", guild.ID, "err", err)
			} else {
				bot.Logger.Debug("voice disconnected successfully", "guild_id", guild.ID)
			}

			// Give Discord time to process disconnect
			time.Sleep(voiceDisconnectWait)

			bot.radioMutex.Lock()
			if bot.radioSessions[guild.ID] == session {
				delete(bot.radioSessions, guild.ID)
			}
			bot.radioMutex.Unlock()
			session.Cancel()
		}()

		if !waitVoiceReady(vc, 5*time.Second, guild.ID, bot) {
			bot.Logger.Error("voice connection did not become ready in time", "guild_id", guild.ID)
			return
		}

		bot.Logger.Info("started streaming from station", "url", station.StreamURL, "name", station.Name, "guild", guild.Name)
		if err := bot.streamRadio(vc, session); err != nil {
			bot.Logger.Error("streaming error", "err", err)
		}
	}()

	bot.NewResponseBuilder(s, i).Success("Starting radio: **"+station.Name+"**", shortDelay)
}

// waitVoiceReady waits for the voice connection to be ready with a timeout.
func waitVoiceReady(vc *discordgo.VoiceConnection, timeout time.Duration, guildID string, bot *Bot) bool {
	deadline := time.Now().Add(timeout)
	attempts := 0
	for {
		if vc.Ready {
			bot.Logger.Debug("voice connection ready", "guild_id", guildID, "attempts", attempts)
			return true
		}
		if time.Now().After(deadline) {
			bot.Logger.Warn("voice connection timeout", "guild_id", guildID, "attempts", attempts, "timeout", timeout)
			return false
		}
		time.Sleep(10 * time.Millisecond)
		attempts++
	}
}

// ===== Command Handlers =====

// handleRadio initiates the radio streaming process.
func (bot *Bot) handleRadio(s *discordgo.Session, i *discordgo.InteractionCreate) {
	bot.radioMutex.Lock()
	sess := bot.radioSessions[i.GuildID]
	bot.radioMutex.Unlock()
	vc := s.VoiceConnections[i.GuildID]

	if sess != nil && vc != nil && vc.Ready {
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

// handleStop stops the current radio stream and disconnects from voice.
func (bot *Bot) handleStop(s *discordgo.Session, i *discordgo.InteractionCreate) {
	vc, err := bot.validateVoiceContextBasic(s, i, true)
	if err != nil {
		bot.NewResponseBuilder(s, i).Error(err.Error(), nil, shortDelay)
		return
	}

	voiceConn := s.VoiceConnections[vc.Guild.ID]
	bot.radioMutex.Lock()
	if session, ok := bot.radioSessions[vc.Guild.ID]; ok && session != nil {
		session.Cancel()
		delete(bot.radioSessions, vc.Guild.ID)
	}
	bot.radioMutex.Unlock()

	_ = voiceConn.Disconnect()
	bot.NewResponseBuilder(s, i).Success("Stopped the radio.", shortDelay)
}

// handleSkip skips the currently playing song on the radio station.
func (bot *Bot) handleSkip(s *discordgo.Session, i *discordgo.InteractionCreate) {
	vc, err := bot.validateVoiceContext(s, i, false)
	if err != nil {
		bot.NewResponseBuilder(s, i).Error(err.Error(), nil, shortDelay)
		return
	}

	// for now, only allow the user who started the stream to skip
	if vc.Session.UserID != i.Member.User.ID {
		bot.NewResponseBuilder(s, i).Error("Only the user who started the stream can skip.", nil, shortDelay)
		return
	}

	err = bot.azureApiClient.SkipCurrentSong(context.Background(), strconv.Itoa(vc.Session.Station.ID))
	if err != nil {
		bot.NewResponseBuilder(s, i).Error("Failed to skip the current song.", err, shortDelay)
		return
	}

	bot.NewResponseBuilder(s, i).Success("Skipped the current song.", shortDelay)
}

// handleNowPlaying shows the currently playing song on the radio station.
func (bot *Bot) handleNowPlaying(s *discordgo.Session, i *discordgo.InteractionCreate) {
	vc, err := bot.validateVoiceContext(s, i, false)
	if err != nil {
		bot.NewResponseBuilder(s, i).Error(err.Error(), nil, shortDelay)
		return
	}

	np, err := bot.azureApiClient.GetStationNowPlaying(context.Background(), strconv.Itoa(vc.Session.Station.ID))
	if err != nil {
		bot.NewResponseBuilder(s, i).Error("Failed to get now playing information.", err, shortDelay)
		return
	}

	bot.NewResponseBuilder(s, i).Success(formatNowPlaying(np), longDelay)
}

// handleRequest handles song requests for the radio station.
func (bot *Bot) handleRequest(s *discordgo.Session, i *discordgo.InteractionCreate) {
	vc, err := bot.validateVoiceContext(s, i, false)
	if err != nil {
		bot.NewResponseBuilder(s, i).Error(err.Error(), nil, shortDelay)
		return
	}

	opts := i.ApplicationCommandData().Options
	songName, err := getStringOption(opts, 0)
	if err != nil {
		bot.NewResponseBuilder(s, i).Error("Song name is required.", err, shortDelay)
		return
	}

	requestableSongs, err := bot.azureApiClient.GetStationRequestableSongs(context.Background(), strconv.Itoa(vc.Session.Station.ID))
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
			bot.requestSong(s, i, song, vc.Session.Station.ID)
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
	vc, err := bot.validateVoiceContext(s, i, false)
	if err != nil {
		bot.NewResponseBuilder(s, i).Error(err.Error(), nil, shortDelay)
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

	if volumeVal == int64(vc.Session.Volume*100) {
		bot.NewResponseBuilder(s, i).Success("Volume is already set to "+strconv.FormatInt(int64(volumeVal), 10)+"%.", shortDelay)
		return
	}

	bot.radioMutex.Lock()
	oldVolume := int(vc.Session.Volume * 100)
	vc.Session.Volume = float64(volumeVal) / 100.0
	stationID := vc.Session.Station.ID
	bot.radioMutex.Unlock()

	// Persist volume change
	if err := bot.sessionStore.Set(i.GuildID, stationID, float64(volumeVal)/100.0); err != nil {
		bot.Logger.Warn("failed to persist volume change", "guild_id", i.GuildID, "err", err)
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

	vc, err := bot.validateVoiceContext(s, i, false)
	if err != nil {
		bot.NewResponseBuilder(s, i).Error(err.Error(), nil, shortDelay)
		return
	}

	requestID := values[0]

	requestableSongs, err := bot.azureApiClient.GetStationRequestableSongs(context.Background(), strconv.Itoa(vc.Session.Station.ID))
	if err != nil {
		bot.NewResponseBuilder(s, i).Error("Failed to get requestable songs.", err, shortDelay)
		return
	}

	selectedSong := findSongByID(requestableSongs, requestID)
	if selectedSong == nil {
		bot.NewResponseBuilder(s, i).Error("Selected song not found.", nil, shortDelay)
		return
	}

	bot.requestSong(s, i, *selectedSong, vc.Session.Station.ID)
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
