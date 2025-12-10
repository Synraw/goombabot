package discord

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/synraw/goombabot/internal/azurecast"
)

const (
	shortDelay  = 5 * time.Second
	mediumDelay = 15 * time.Second
	longDelay   = 30 * time.Second
	maxLabelLen = 100
)

// truncateLabel ensures select option labels meet Discord's 1–100 char limit.
func truncateLabel(label string) string {
	r := []rune(label)
	if len(r) <= maxLabelLen {
		return label
	}
	return string(r[:maxLabelLen-1]) + "…"
}

// helper functions

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

// getUserVoiceChannelID returns the voice channel ID the user is currently in, or "" if not found.
func getUserVoiceChannelID(guild *discordgo.Guild, userID string) string {
	for _, vs := range guild.VoiceStates {
		if vs.UserID == userID {
			return vs.ChannelID
		}
	}
	return ""
}

// respondWithError logs, sends a response, and deletes it after delay.
func (bot *Bot) respondWithError(s *discordgo.Session, i *discordgo.InteractionCreate, msg string, err error, delay time.Duration) {
	if bot.Logger != nil && err != nil {
		bot.Logger.Warn(msg, "err", err)
	}
	_ = s.InteractionRespond(i.Interaction, createResponse(msg))
	deleteMessageAfter(s, i, delay)
}

// requestSong handles the process of requesting a song on the radio station.
func (bot *Bot) requestSong(s *discordgo.Session, i *discordgo.InteractionCreate, song azurecast.StationSongRequest, stationID int) {
	// Request the song
	response, err := bot.azureApiClient.RequestStationSong(context.Background(), strconv.Itoa(stationID), song)
	if err != nil {
		bot.respondWithError(s, i, "Failed to request the song.", err, shortDelay)
		return
	}

	// Check response success
	if !response.Success {
		bot.respondWithError(s, i, "Failed to request the song: "+response.Message, nil, shortDelay)
		return
	}

	_ = s.InteractionRespond(i.Interaction, createResponse("Requested song **"+song.Song.Text+"**."))
	deleteMessageAfter(s, i, mediumDelay)
}

// runRadioStream handles the process of joining voice and streaming the radio.
func (bot *Bot) runRadioStream(s *discordgo.Session, i *discordgo.InteractionCreate, station RadioStation) {
	guild, err := s.State.Guild(i.GuildID)
	if err != nil {
		bot.respondWithError(s, i, "Could not get guild.", err, shortDelay)
		return
	}

	voiceChannelID := getUserVoiceChannelID(guild, i.Member.User.ID)
	if voiceChannelID == "" {
		bot.respondWithError(s, i, "You must be in a voice channel.", nil, shortDelay)
		return
	}

	vc, err := s.ChannelVoiceJoin(guild.ID, voiceChannelID, false, true)
	if err != nil {
		bot.respondWithError(s, i, "Failed to join voice channel.", err, shortDelay)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	bot.radioMutex.Lock()
	bot.radioSessions[guild.ID] = &StreamSession{
		Context: ctx,
		Cancel:  cancel,
		UserID:  i.Member.User.ID,
		Station: &station,
	}
	bot.radioMutex.Unlock()

	go func() {
		defer func() {
			_ = vc.Disconnect()
			bot.radioMutex.Lock()
			if session, ok := bot.radioSessions[guild.ID]; ok && session != nil {
				session.Cancel()
				delete(bot.radioSessions, guild.ID)
			}
			bot.radioMutex.Unlock()
		}()
		bot.Logger.Info("started streaming from station", "url", station.StreamURL, "name", station.Name, "guild", guild.Name)
		if station.IsOpus {
			if err := bot.streamRadioDirectOpus(vc, bot.radioSessions[guild.ID]); err != nil {
				bot.Logger.Error("streaming opus error", "err", err)
				return
			}
		} else {
			if err := bot.streamRadioWithFFmpeg(vc, bot.radioSessions[guild.ID]); err != nil {
				bot.Logger.Error("streaming error", "err", err)
				return
			}
		}
		bot.Logger.Info("stopped streaming from station", "name", station.Name, "guild", guild.Name)
	}()

	_ = s.InteractionRespond(i.Interaction, createResponse("Starting radio: **"+station.Name+"**"))
	deleteMessageAfter(s, i, shortDelay)
}

// command handlers

// handleRadio initiates the radio streaming process.
func (bot *Bot) handleRadio(s *discordgo.Session, i *discordgo.InteractionCreate) {
	// check if already in a voice channel and streaming
	if _, ok := s.VoiceConnections[i.GuildID]; ok && bot.radioSessions[i.GuildID] != nil {
		_ = s.InteractionRespond(i.Interaction, createResponse("Already streaming in a voice channel. Use /stop to stop the current stream first."))
		deleteMessageAfter(s, i, shortDelay)
		return
	}

	// if only one station, select it by default
	if len(bot.radioStations) == 1 {
		for _, station := range bot.radioStations {
			bot.runRadioStream(s, i, station)
		}
		return
	}

	minValues := 1
	stationSelect := discordgo.SelectMenu{
		CustomID:    "radio_station_select",
		Placeholder: "Select a station",
		MinValues:   &minValues,
		MaxValues:   len(bot.radioStations),
		Options:     []discordgo.SelectMenuOption{}, // start empty
	}

	// Populate options
	for id, station := range bot.radioStations {
		stationSelect.Options = append(stationSelect.Options, discordgo.SelectMenuOption{
			Label: truncateLabel(station.Name),
			Value: strconv.Itoa(id),
		})
	}

	// send the select menu as a response
	resp := &discordgo.InteractionResponseData{
		Content: "Select a radio station:",
		Components: []discordgo.MessageComponent{
			discordgo.ActionsRow{
				Components: []discordgo.MessageComponent{
					stationSelect,
				},
			},
		},
		Flags: discordgo.MessageFlagsEphemeral,
	}

	if err := s.InteractionRespond(i.Interaction, createResponseEx(resp)); err != nil {
		bot.Logger.Warn("failed to send radio select", "err", err)
	}
}

// handleStop stops the current radio stream and disconnects from voice.
func (bot *Bot) handleStop(s *discordgo.Session, i *discordgo.InteractionCreate) {
	guild, err := s.State.Guild(i.GuildID)
	if err != nil {
		bot.respondWithError(s, i, "Could not get guild.", err, shortDelay)
		return
	}

	voiceChannelID := getUserVoiceChannelID(guild, i.Member.User.ID)
	if voiceChannelID == "" {
		bot.respondWithError(s, i, "You must be in a voice channel.", nil, shortDelay)
		return
	}

	if s.VoiceConnections[guild.ID] == nil {
		bot.respondWithError(s, i, "Not connected to a voice channel.", nil, shortDelay)
		return
	}

	if vc, ok := s.VoiceConnections[guild.ID]; ok {
		bot.radioMutex.Lock()
		if session, ok := bot.radioSessions[guild.ID]; ok && session != nil {
			session.Cancel()
			delete(bot.radioSessions, guild.ID)
		}
		bot.radioMutex.Unlock()
		_ = vc.Disconnect()
	}

	_ = s.InteractionRespond(i.Interaction, createResponse("Stopped the radio."))
	deleteMessageAfter(s, i, shortDelay)
}

// handleSkip skips the currently playing song on the radio station.
func (bot *Bot) handleSkip(s *discordgo.Session, i *discordgo.InteractionCreate) {
	guild, err := s.State.Guild(i.GuildID)
	if err != nil {
		bot.respondWithError(s, i, "Could not get guild.", err, shortDelay)
		return
	}

	voiceChannelID := getUserVoiceChannelID(guild, i.Member.User.ID)
	if voiceChannelID == "" {
		bot.respondWithError(s, i, "You must be in a voice channel.", nil, shortDelay)
		return
	}

	if s.VoiceConnections[guild.ID] == nil {
		bot.respondWithError(s, i, "Not connected to a voice channel.", nil, shortDelay)
		return
	}

	session, ok := bot.radioSessions[guild.ID]
	if !ok {
		bot.respondWithError(s, i, "No active radio session to skip.", nil, shortDelay)
		return
	}

	if session.UserID != i.Member.User.ID {
		bot.respondWithError(s, i, "Only the user who started the stream can skip.", nil, shortDelay)
		return
	}

	err = bot.azureApiClient.SkipCurrentSong(context.Background(), strconv.Itoa(session.Station.ID))
	if err != nil {
		bot.respondWithError(s, i, "Failed to skip the current song.", err, shortDelay)
		return
	}

	_ = s.InteractionRespond(i.Interaction, createResponse("Skipped the current song."))
	deleteMessageAfter(s, i, shortDelay)
}

// handleNowPlaying shows the currently playing song on the radio station.
func (bot *Bot) handleNowPlaying(s *discordgo.Session, i *discordgo.InteractionCreate) {
	guild, err := s.State.Guild(i.GuildID)
	if err != nil {
		bot.respondWithError(s, i, "Could not get guild.", err, shortDelay)
		return
	}

	voiceChannelID := getUserVoiceChannelID(guild, i.Member.User.ID)
	if voiceChannelID == "" {
		bot.respondWithError(s, i, "You must be in a voice channel.", nil, shortDelay)
		return
	}

	if s.VoiceConnections[guild.ID] == nil {
		bot.respondWithError(s, i, "Not connected to a voice channel.", nil, shortDelay)
		return
	}

	session, ok := bot.radioSessions[guild.ID]
	if !ok {
		bot.respondWithError(s, i, "No active radio session.", nil, shortDelay)
		return
	}

	np, err := bot.azureApiClient.GetStationNowPlaying(context.Background(), strconv.Itoa(session.Station.ID))
	if err != nil {
		bot.respondWithError(s, i, "Failed to get now playing information.", err, shortDelay)
		return
	}

	response := "Now Playing:\n"
	if np.NowPlaying.Song.Title != "" {
		response += "**" + np.NowPlaying.Song.Title + "**"
	} else {
		response += "Unknown Title"
	}
	if np.NowPlaying.Song.Artist != "" {
		response += " by **" + np.NowPlaying.Song.Artist + "**"
	} else {
		response += " by Unknown Artist"
	}
	if np.NowPlaying.Song.Album != "" {
		response += " from the album **" + np.NowPlaying.Song.Album + "**"
	}
	_ = s.InteractionRespond(i.Interaction, createResponse(response))
	deleteMessageAfter(s, i, longDelay)
}

func (bot *Bot) handleRequest(s *discordgo.Session, i *discordgo.InteractionCreate) {
	guild, err := s.State.Guild(i.GuildID)
	if err != nil {
		bot.respondWithError(s, i, "Could not get guild.", err, shortDelay)
		return
	}

	voiceChannelID := getUserVoiceChannelID(guild, i.Member.User.ID)
	if voiceChannelID == "" {
		bot.respondWithError(s, i, "You must be in a voice channel.", nil, shortDelay)
		return
	}

	if s.VoiceConnections[guild.ID] == nil {
		bot.respondWithError(s, i, "Not connected to a voice channel.", nil, shortDelay)
		return
	}

	// get current session
	session, ok := bot.radioSessions[guild.ID]
	if !ok {
		bot.respondWithError(s, i, "No active radio session.", nil, shortDelay)
		return
	}

	// Extract song name early
	opts := i.ApplicationCommandData().Options
	if len(opts) == 0 {
		bot.respondWithError(s, i, "Song name is required.", nil, shortDelay)
		return
	}
	songName := opts[0].StringValue() // only one option for request

	// Get requestable songs
	requestableSongs, err := bot.azureApiClient.GetStationRequestableSongs(context.Background(), strconv.Itoa(session.Station.ID))
	if err != nil {
		bot.respondWithError(s, i, "Failed to get requestable songs.", err, shortDelay)
		return
	}

	// Find matching song (case-insensitive)
	matchingSongs := map[string]azurecast.StationSongRequest{}
	songNameLower := strings.ToLower(songName)
	for i := range requestableSongs {
		if strings.Contains(strings.ToLower(requestableSongs[i].Song.Text), songNameLower) {
			matchingSongs[requestableSongs[i].RequestID] = requestableSongs[i]
		}
	}

	// If no song found
	if len(matchingSongs) == 0 {
		bot.respondWithError(s, i, "Song not found in requestable list.", nil, shortDelay)
		return
	}

	// If only one match, request it directly
	if len(matchingSongs) == 1 {
		for _, song := range matchingSongs {
			bot.requestSong(s, i, song, session.Station.ID)
		}
		return
	}

	// Multiple matches - ask user to select
	minValues := 1
	songSelect := discordgo.SelectMenu{
		CustomID:    "song_request_select",
		Placeholder: "Select a song to request",
		MinValues:   &minValues,
		MaxValues:   1,
		Options:     []discordgo.SelectMenuOption{}, // start empty
	}

	// Populate options (limit to 25 options due to Discord API limits)
	count := 0
	for requestID, song := range matchingSongs {
		if count >= 25 {
			break
		}
		songSelect.Options = append(songSelect.Options, discordgo.SelectMenuOption{
			Label: truncateLabel(song.Song.Text),
			Value: requestID,
		})
		count++
	}

	// send the select menu as a response
	resp := &discordgo.InteractionResponseData{
		Content: "Multiple songs found. Please select one:",
		Components: []discordgo.MessageComponent{
			discordgo.ActionsRow{
				Components: []discordgo.MessageComponent{
					songSelect,
				},
			},
		},
		Flags: discordgo.MessageFlagsEphemeral,
	}

	if err := s.InteractionRespond(i.Interaction, createResponseEx(resp)); err != nil {
		bot.Logger.Warn("failed to send song select", "err", err)
	}
}

// handleComponent routes interaction component events to the appropriate handler.
func (bot *Bot) handleComponent(s *discordgo.Session, i *discordgo.InteractionCreate) {
	switch i.MessageComponentData().CustomID {
	case "radio_station_select":
		bot.handleRadioSelect(s, i)
	case "song_request_select":
		bot.handleSongRequestSelect(s, i)
	}
}

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

// handleRadioSelect processes the station selection and starts streaming.
func (bot *Bot) handleRadioSelect(s *discordgo.Session, i *discordgo.InteractionCreate) {
	values := i.MessageComponentData().Values
	if len(values) == 0 {
		_ = s.InteractionRespond(i.Interaction, createResponse("No station selected."))
		deleteMessageAfter(s, i, shortDelay)
		return
	}

	stationID, _ := strconv.Atoi(values[0])
	station := bot.radioStations[stationID]

	bot.runRadioStream(s, i, station)
}

// handleSongRequestSelect processes the song selection and requests it.
func (bot *Bot) handleSongRequestSelect(s *discordgo.Session, i *discordgo.InteractionCreate) {
	values := i.MessageComponentData().Values
	if len(values) == 0 {
		_ = s.InteractionRespond(i.Interaction, createResponse("No song selected."))
		deleteMessageAfter(s, i, shortDelay)
		return
	}

	guild, err := s.State.Guild(i.GuildID)
	if err != nil {
		bot.respondWithError(s, i, "Could not get guild.", err, shortDelay)
		return
	}

	voiceChannelID := getUserVoiceChannelID(guild, i.Member.User.ID)
	if voiceChannelID == "" {
		bot.respondWithError(s, i, "You must be in a voice channel.", nil, shortDelay)
		return
	}

	if s.VoiceConnections[guild.ID] == nil {
		bot.respondWithError(s, i, "Not connected to a voice channel.", nil, shortDelay)
		return
	}

	// get current session
	session, ok := bot.radioSessions[guild.ID]
	if !ok {
		bot.respondWithError(s, i, "No active radio session.", nil, shortDelay)
		return
	}

	requestID := values[0]

	// Get requestable songs to find the selected one.
	// We re-fetch here rather than caching from handleRequest to ensure we have
	// the latest song list, as there may be a delay between showing the menu and selection.
	requestableSongs, err := bot.azureApiClient.GetStationRequestableSongs(context.Background(), strconv.Itoa(session.Station.ID))
	if err != nil {
		bot.respondWithError(s, i, "Failed to get requestable songs.", err, shortDelay)
		return
	}

	// Find the song with matching requestID
	var selectedSong *azurecast.StationSongRequest
	for idx := range requestableSongs {
		if requestableSongs[idx].RequestID == requestID {
			selectedSong = &requestableSongs[idx]
			break
		}
	}

	if selectedSong == nil {
		bot.respondWithError(s, i, "Selected song not found.", nil, shortDelay)
		return
	}

	bot.requestSong(s, i, *selectedSong, session.Station.ID)
}
