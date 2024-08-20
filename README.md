# Telchar Smith Bot 

![Telchar](https://tolkiengateway.net/w/images/c/c8/Donato_Giancola_-_Telchar_forging_Narsil.jpg)

Telchar is a bot that updates you in real-time when your Stake Pool forges a new block on the Cardano blockchain to your telegram channel utilizing [Adder](https://github.com/blinklabs-io/adder) a Cardano tailing tool provided by [Blinklabs io](https://github.com/blinklabs-io).

## Features
- Real-time updates on forged blocks with no API key required just listen to any Cardano node
- Minimal API calls for efficiency
- Can be run on any type of OS and architecture
- Easy to set up and run

## Requirements
- A Cardano node running on your machine or a remote server, but you can also use a public node address and port as well. See example config.yaml file for more details.
- A telegram bot token from the [BotFather](https://core.telegram.org/bots#6-botfather)
- A telegram channel ID to send messages to. You can get this from the channel info page or use one of the following methods [here](https://gist.github.com/mraaroncruz/e76d19f7d61d59419002db54030ebe35?permalink_comment_id=4877872) to get the channel ID.