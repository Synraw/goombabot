## Goomba Bot for Discord

Goombabot is a discord bot for discord allowing for streaming from an Azurecast radio into a voice channel.
It intergrates smartly into the API of your azurecast server allowing for seemless station / endpoint selection, as well as providing administration
features for whitelisted "moderator" users. This includes stuff like the ability to force a playlist to begin playing, skipping a song, or removing the currently playing song from the current playlist.

Rough plan of what needs to be required initially:

- Fetch station and endpoints from azurecast server
- Allow user to use a discord "application command" to choose a station to begin playing in their current voice channel
- Can play the station audio in the discord voice channel. For starters we will try limit this to OPUS azurecast endpoints only to simplify things

List of possible commands:

- radio: play the radio (arguments tbd)
- 

