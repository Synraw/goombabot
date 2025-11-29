package discord

import (
	"strconv"
	"time"

	"github.com/bwmarrin/discordgo"
)

// helper functions

func deleteMessageAfter(s *discordgo.Session, i *discordgo.InteractionCreate, delay time.Duration) {
	go func() {
		time.Sleep(delay)
		_ = s.InteractionResponseDelete(i.Interaction)
	}()
}

func buildInteractionResponse(content string) *discordgo.InteractionResponse {
	return &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: content,
		},
	}
}

func buildInteractionResponseEx(i *discordgo.InteractionResponseData) *discordgo.InteractionResponse {
	return &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: i,
	}
}

// command handlers

func (bot *Bot) handleRadio(s *discordgo.Session, i *discordgo.InteractionCreate) {
	minValues := 1

	stationSelect := discordgo.SelectMenu{
		CustomID:    "radio_station_select",
		Placeholder: "Select a station",
		MinValues:   &minValues,
		MaxValues:   len(bot.radioStations),
		Options:     []discordgo.SelectMenuOption{}, // start empty
	}

	// check if already in a voice channel and streaming
	if _, ok := s.VoiceConnections[i.GuildID]; ok || bot.radioCancel[i.GuildID] != nil {
		_ = s.InteractionRespond(i.Interaction, buildInteractionResponse("Already streaming in a voice channel. Use /stop to stop the current stream first."))
		deleteMessageAfter(s, i, 5*time.Second)
		return
	}

	// if only one station, select it by default
	if len(bot.radioStations) == 1 {
		station := bot.radioStations[1]

		// Find the voice channel the user is in
		guild, err := s.State.Guild(i.GuildID)
		if err != nil {
			bot.Logger.Warn("failed to get guild", "err", err)
			_ = s.InteractionRespond(i.Interaction, buildInteractionResponse("Could not get guild."))
			deleteMessageAfter(s, i, 5*time.Second)
			return
		}

		// Find the voice channel the user is in
		var voiceChannelID string
		for _, vs := range guild.VoiceStates {
			if vs.UserID == i.Member.User.ID {
				voiceChannelID = vs.ChannelID
				break
			}
		}

		if voiceChannelID == "" {
			_ = s.InteractionRespond(i.Interaction, buildInteractionResponse("You must be in a voice channel."))
			deleteMessageAfter(s, i, 5*time.Second)
			return
		}

		// Join voice
		vc, err := s.ChannelVoiceJoin(guild.ID, voiceChannelID, false, false)
		if err != nil {
			bot.Logger.Warn("failed to join voice", "err", err)
			_ = s.InteractionRespond(i.Interaction, buildInteractionResponse("Failed to join voice channel."))
			deleteMessageAfter(s, i, 5*time.Second)
			return
		}

		// Start streaming
		done := make(chan struct{})
		bot.mutex.Lock()
		bot.radioCancel[guild.ID] = done
		bot.mutex.Unlock()

		go func() {
			defer func() {
				_ = vc.Disconnect()
				bot.mutex.Lock()
				delete(bot.radioCancel, guild.ID)
				bot.mutex.Unlock()
			}()
			bot.Logger.Debug("now streaming from station", "url", station.StreamURL, "name", station.Name, "guild", guild.Name)
			_ = bot.streamRadioWithFFmpeg(vc, station.StreamURL, done)
			bot.Logger.Debug("stopped streaming from station", "name", station.Name, "guild", guild.Name)
		}()

		_ = s.InteractionRespond(i.Interaction, buildInteractionResponse("Starting radio: **"+station.Name+"**"))
		deleteMessageAfter(s, i, 5*time.Second)
		return
	}

	// Populate options
	for id, station := range bot.radioStations {
		stationSelect.Options = append(stationSelect.Options, discordgo.SelectMenuOption{
			Label: station.Name,
			Value: strconv.Itoa(id),
		})
	}

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

	if err := s.InteractionRespond(i.Interaction, buildInteractionResponseEx(resp)); err != nil {
		bot.Logger.Warn("failed to send radio select", "err", err)
	}
}

func (bot *Bot) handleStop(s *discordgo.Session, i *discordgo.InteractionCreate) {
	// Find the voice channel the user is in
	guild, err := s.State.Guild(i.GuildID)
	if err != nil {
		bot.Logger.Warn("failed to get guild", "err", err)
		_ = s.InteractionRespond(i.Interaction, buildInteractionResponse("Could not get guild."))
		deleteMessageAfter(s, i, 5*time.Second)
		return
	}

	// Find the voice channel the user is in
	var voiceChannelID string
	for _, vs := range guild.VoiceStates {
		if vs.UserID == i.Member.User.ID {
			voiceChannelID = vs.ChannelID
			break
		}
	}

	if voiceChannelID == "" {
		_ = s.InteractionRespond(i.Interaction, buildInteractionResponse("You must be in a voice channel."))
		deleteMessageAfter(s, i, 5*time.Second)
		return
	}

	if s.VoiceConnections[guild.ID] == nil {
		_ = s.InteractionRespond(i.Interaction, buildInteractionResponse("Not connected to a voice channel."))
		deleteMessageAfter(s, i, 5*time.Second)
		return
	}

	// Disconnect from voice
	if vc, ok := s.VoiceConnections[guild.ID]; ok {
		bot.mutex.Lock()
		close(bot.radioCancel[guild.ID])  // Signal the stream to stop
		delete(bot.radioCancel, guild.ID) // Remove from map
		bot.mutex.Unlock()
		_ = vc.Disconnect()
	}

	_ = s.InteractionRespond(i.Interaction, buildInteractionResponse("Stopped the radio."))

	deleteMessageAfter(s, i, 5*time.Second)
}

func (bot *Bot) handleComponent(s *discordgo.Session, i *discordgo.InteractionCreate) {
	switch i.MessageComponentData().CustomID {
	case "radio_station_select":
		bot.handleRadioSelect(s, i)
	}
}

func (bot *Bot) handleRadioSelect(s *discordgo.Session, i *discordgo.InteractionCreate) {
	values := i.MessageComponentData().Values
	if len(values) == 0 {
		_ = s.InteractionRespond(i.Interaction, buildInteractionResponse("No station selected."))
		deleteMessageAfter(s, i, 5*time.Second)
		return
	}

	stationID, _ := strconv.Atoi(values[0])
	station := bot.radioStations[stationID]

	// Find the voice channel the user is in
	guild, err := s.State.Guild(i.GuildID)
	if err != nil {
		bot.Logger.Warn("failed to get guild", "err", err)
		_ = s.InteractionRespond(i.Interaction, buildInteractionResponse("Could not get guild."))
		deleteMessageAfter(s, i, 5*time.Second)
		return
	}

	var voiceChannelID string
	for _, vs := range guild.VoiceStates {
		if vs.UserID == i.Member.User.ID {
			voiceChannelID = vs.ChannelID
			break
		}
	}

	if voiceChannelID == "" {
		_ = s.InteractionRespond(i.Interaction, buildInteractionResponse("You must be in a voice channel."))
		deleteMessageAfter(s, i, 5*time.Second)
		return
	}

	// Join voice
	vc, err := s.ChannelVoiceJoin(guild.ID, voiceChannelID, false, false)
	if err != nil {
		bot.Logger.Warn("failed to join voice", "err", err)
		_ = s.InteractionRespond(i.Interaction, buildInteractionResponse("Failed to join voice channel."))
		deleteMessageAfter(s, i, 5*time.Second)
		return
	}

	done := make(chan struct{})

	bot.mutex.Lock()
	bot.radioCancel[guild.ID] = done
	bot.mutex.Unlock()

	go func() {
		defer func() {
			_ = vc.Disconnect()
			bot.mutex.Lock()
			delete(bot.radioCancel, guild.ID)
			bot.mutex.Unlock()
		}()
		bot.Logger.Debug("now streaming from station", "url", station.StreamURL, "name", station.Name, "guild", guild.Name)
		_ = bot.streamRadioWithFFmpeg(vc, station.StreamURL, done)
		bot.Logger.Debug("stopped streaming from station", "name", station.Name, "guild", guild.Name)
	}()

	_ = s.InteractionRespond(i.Interaction, buildInteractionResponse("Starting radio: **"+station.Name+"**"))
	deleteMessageAfter(s, i, 5*time.Second)
}
