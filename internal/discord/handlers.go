package discord

import (
	"strconv"

	"github.com/bwmarrin/discordgo"
)

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

func (bot *Bot) handleComponent(s *discordgo.Session, i *discordgo.InteractionCreate) {
	switch i.MessageComponentData().CustomID {
	case "radio_station_select":
		bot.handleRadioSelect(s, i)
	}
}

func (bot *Bot) handleRadioSelect(s *discordgo.Session, i *discordgo.InteractionCreate) {
	// Only allow the user who invoked the original command
	// to use this select menu.
	originalUserID := i.Message.Interaction.User.ID // who ran /radio
	clickUserID := i.Member.User.ID                 // who clicked

	if clickUserID != originalUserID {
		// Reply ephemerally to the clicker: "this isn't your menu"
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "Only the user who started this radio command can select a station.",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
		return
	}

	values := i.MessageComponentData().Values
	if len(values) == 0 {
		_ = s.InteractionRespond(i.Interaction, buildInteractionResponse("No station selected."))
		return
	}

	stationID, _ := strconv.Atoi(values[0])
	station := bot.radioStations[stationID]

	// Find the voice channel the user is in
	guild, err := s.State.Guild(i.GuildID)
	if err != nil {
		bot.Logger.Warn("failed to get guild", "err", err)
		_ = s.InteractionRespond(i.Interaction, buildInteractionResponse("Could not get guild."))
		return
	}

	var voiceChannelID string
	for _, vs := range guild.VoiceStates {
		if vs.UserID == clickUserID {
			voiceChannelID = vs.ChannelID
			break
		}
	}
	if voiceChannelID == "" {
		_ = s.InteractionRespond(i.Interaction, buildInteractionResponse("You must be in a voice channel."))
		return
	}

	// Join voice
	vc, err := s.ChannelVoiceJoin(guild.ID, voiceChannelID, false, false)
	if err != nil {
		bot.Logger.Warn("failed to join voice", "err", err)
		_ = s.InteractionRespond(i.Interaction, buildInteractionResponse("Failed to join voice channel."))
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
		bot.Logger.Debug("now streaming from station", "url", station.StreamURL, "name", station.Name)
		_ = streamRadioWithFFmpeg(vc, station.StreamURL, done)
	}()

	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: "Starting radio: **" + station.Name + "**",
		},
	})
}
