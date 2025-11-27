package discord

import "github.com/bwmarrin/discordgo"

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

func handlePing(s *discordgo.Session, i *discordgo.InteractionCreate) {
	_ = s.InteractionRespond(i.Interaction, buildInteractionResponse("Pong!"))
}

func handleRadio(s *discordgo.Session, i *discordgo.InteractionCreate) {
	// TODO: implement:
	// - fetch Azurecast stations/endpoints
	// - pick station (from options or via follow‑up interaction)
	// - join voice and start streaming OPUS endpoint
	_ = s.InteractionRespond(i.Interaction, buildInteractionResponse("Radio command not implemented yet."))
}
