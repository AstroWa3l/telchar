package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/blinklabs-io/snek/event"
	filter_event "github.com/blinklabs-io/snek/filter/event"
	"github.com/blinklabs-io/snek/input/chainsync"
	output_embedded "github.com/blinklabs-io/snek/output/embedded"
	"github.com/blinklabs-io/snek/pipeline"
	"github.com/gorilla/websocket"
	"github.com/spf13/viper"
	telebot "gopkg.in/tucnak/telebot.v2"
)

// Channel to broadcast block events to connected clients
var clients = make(map[*websocket.Conn]bool) // connected clients
var broadcast = make(chan interface{})       // broadcast channel

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

// Telegram bot
var bot *telebot.Bot

type BlockEvent struct {
	Type      string                 `json:"type"`
	Timestamp string                 `json:"timestamp"`
	Context   chainsync.BlockContext `json:"context"`
	Payload   chainsync.BlockEvent   `json:"payload"`
}

type RollbackEvent struct {
	Type      string                  `json:"type"`
	Timestamp string                  `json:"timestamp"`
	Payload   chainsync.RollbackEvent `json:"payload"`
}

type TransactionContext struct {
	BlockNumber     int    `json:"blockNumber"`
	SlotNumber      int    `json:"slotNumber"`
	TransactionHash string `json:"transactionHash"`
	TransactionIdx  int    `json:"transactionIdx"`
	NetworkMagic    int    `json:"networkMagic"`
}

type Asset struct {
	Name        string `json:"name"`
	NameHex     string `json:"nameHex"`
	PolicyId    string `json:"policyId"`
	Fingerprint string `json:"fingerprint"`
	Amount      int    `json:"amount"`
}

type TransactionOutput struct {
	Address string  `json:"address"`
	Amount  int     `json:"amount"`
	Assets  []Asset `json:"assets,omitempty"`
}

type TransactionPayload struct {
	BlockHash       string                 `json:"blockHash"`
	TransactionCbor string                 `json:"transactionCbor"`
	Inputs          []string               `json:"inputs"`
	Outputs         []TransactionOutput    `json:"outputs"`
	Metadata        map[string]interface{} `json:"metadata"`
	Fee             int                    `json:"fee"`
	Ttl             int                    `json:"ttl"`
}

type TransactionEvent struct {
	Type      string             `json:"type"`
	Timestamp string             `json:"timestamp"`
	Context   TransactionContext `json:"context"`
	Payload   TransactionPayload `json:"payload"`
}

// Indexer struct to manage the Snek pipeline and block events
type Indexer struct {
	pipeline         *pipeline.Pipeline
	blockEvent       BlockEvent
	bot              *telebot.Bot
	transactionEvent TransactionEvent
	rollbackEvent    RollbackEvent
	nodeAddress      string
	poolId           string
	telegramChannel  string
	telegramToken    string
}

// Singleton instance of the Indexer
var globalIndexer = &Indexer{}

// Start the Snek pipeline and handle block events
func (i *Indexer) Start() error {

	viper.SetConfigName("config") // name of config file (without extension)
	viper.AddConfigPath(".")      // look for config in the working directory

	e := viper.ReadInConfig() // Find and read the config file
	if e != nil {             // Handle errors reading the config file
		log.Fatalf("Error while reading config file %s", e)

	}

	i.nodeAddress = viper.GetString("nodeAddress")
	i.poolId = viper.GetString("poolId")
	i.telegramChannel = viper.GetString("telegram.channel")
	i.telegramToken = viper.GetString("telegram.token")

	// Initialize the bot
	var err error
	i.bot, err = telebot.NewBot(telebot.Settings{
		Token:  i.telegramToken,
		Poller: &telebot.LongPoller{Timeout: 10 * time.Second},
	})

	if err != nil {
		log.Fatalf("failed to start bot: %s", err)
	}

	node := chainsync.WithAddress(i.nodeAddress)
	inputOpts := []chainsync.ChainSyncOptionFunc{
		node,
		chainsync.WithNetworkMagic(764824073),
		chainsync.WithIntersectTip(true),
		chainsync.WithAutoReconnect(true),
	}
	// Create a new pipeline
	i.pipeline = pipeline.New()

	// Configure ChainSync input
	input_chainsync := chainsync.New(inputOpts...)
	i.pipeline.AddInput(input_chainsync)

	// Configure filter to handle events
	filterEvent := filter_event.New(filter_event.WithTypes([]string{"chainsync.block", "chainsync.rollback"}))
	i.pipeline.AddFilter(filterEvent)

	// Configure embedded output with callback function
	output := output_embedded.New(output_embedded.WithCallbackFunc(i.handleEvent))
	i.pipeline.AddOutput(output)

	// Start the pipeline
	log.Println("Starting the pipeline...")
	if err := i.pipeline.Start(); err != nil {
		log.Fatalf("failed to start pipeline: %s\n", err)
	}

	// Start error handler in a goroutine
	go func() {
		for {
			err, ok := <-i.pipeline.ErrorChan()
			if ok {
				log.Printf("pipeline failed: %s\n", err)
				log.Println("Restarting pipeline...")
				if err := i.pipeline.Start(); err != nil {
					log.Fatalf("failed to restart pipeline: %s\n", err)
				}
			}
		}
	}()

	return nil
}

// Handle block events received from the Snek pipeline
func (i *Indexer) handleEvent(event event.Event) error {

	defer func() {
		if r := recover(); r != nil {
			log.Println("Recovered in handleEvent", r)
		}
	}()

	// Marshal the event to JSON
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}

	// Unmarshal the event to get the event type
	var getEvent map[string]interface{}
	errr := json.Unmarshal(data, &getEvent)
	if errr != nil {
		return err
	}

	eventType, ok := getEvent["type"].(string)
	if !ok {
		return fmt.Errorf("failed to get event type")
	}

	channel, err := i.bot.ChatByID(i.telegramChannel) // replace 'yourchannel' with your channel's username
	if err != nil {
		panic(err) // You should add better error handling than this!
	}

	switch eventType {
	case "chainsync.block":
		var blockEvent BlockEvent
		err := json.Unmarshal(data, &blockEvent)
		if err != nil {
			return err
		}

		parsedTime, err := time.Parse(time.RFC3339, blockEvent.Timestamp)
		if err == nil {
			blockEvent.Timestamp = parsedTime.Format("January 2, 2006 15:04:05 MST")
			fmt.Printf("Time: %s\n", blockEvent.Timestamp)
		}

		if blockEvent.Payload.IssuerVkey == i.poolId {
			msg := fmt.Sprintf("OTG New block received: %d\nHash: %s\nIssuer: %s\nTime: %s", blockEvent.Context.BlockNumber,
				blockEvent.Payload.BlockHash, blockEvent.Payload.IssuerVkey, blockEvent.Timestamp)
			_, err = i.bot.Send(channel, msg)
			if err != nil {
				log.Printf("failed to send Telegram message: %s", err)
			}
		}
		// } else {
		// 	msg := fmt.Sprintf("New block received: %d\nHash: %s\nIssuer: %s\nTime: %s", blockEvent.Context.BlockNumber,
		// 		blockEvent.Payload.BlockHash, blockEvent.Payload.IssuerVkey, blockEvent.Timestamp)
		// 	_, err = i.bot.Send(channel, msg)
		// 	if err != nil {
		// 		log.Printf("failed to send Telegram message: %s", err)
		// 	}
		// }

		// Update the currentEvent field in the Indexer
		i.blockEvent = blockEvent

		fmt.Printf("Received Event: %+v\n", blockEvent)

		// Send the block event to the WebSocket clients
		broadcast <- blockEvent

	case "chainsync.rollback":
		var rollbackEvent RollbackEvent
		err := json.Unmarshal(data, &rollbackEvent)
		if err != nil {
			return err
		}

		parsedTime, err := time.Parse(time.RFC3339, rollbackEvent.Timestamp)
		if err == nil {
			rollbackEvent.Timestamp = parsedTime.Format("January 2, 2006 15:04:05 MST")
		}

		i.rollbackEvent = rollbackEvent

		fmt.Printf("Received Event: %+v\n", rollbackEvent)

		broadcast <- rollbackEvent

	case "chainsync.transaction":
		// fmt.Println("Received transaction event:", string(data))

		var transactionEvent TransactionEvent

		errr := json.Unmarshal(data, &transactionEvent)
		if errr != nil {
			log.Printf("error unmarshalling transaction event: %v, data: %s", errr, string(data))
			return fmt.Errorf("error unmarshalling transaction event: %v", errr)
		}

		parsedTime, err := time.Parse(time.RFC3339, transactionEvent.Timestamp)
		if err == nil {
			transactionEvent.Timestamp = parsedTime.Format("January 2, 2006 15:04:05 MST")
		}

		i.transactionEvent = transactionEvent

		fmt.Printf("Received Transaction Event at time: %s\n", transactionEvent.Timestamp)

		broadcast <- transactionEvent
	}

	// Start error handler in a goroutine
	go func() {
		for {
			err, ok := <-i.pipeline.ErrorChan()
			if ok {
				log.Printf("pipeline failed: %s\n", err)
				log.Println("Restarting pipeline...")
				if err := i.pipeline.Start(); err != nil {
					log.Fatalf("failed to restart pipeline: %s\n", err)
				}
			}
		}
	}()

	return nil
}

func handleConnections(w http.ResponseWriter, r *http.Request) {
	// Upgrade initial GET request to a websocket
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Fatal(err)
	}
	// Make sure we close the connection when the function returns
	defer ws.Close()

	// Register our new client
	clients[ws] = true

	for {
		var msg interface{}
		// Read in a new message as JSON and map it to a Message object
		err := ws.ReadJSON(&msg)
		if err != nil {
			log.Printf("error: %v", err)
			delete(clients, ws)
			break
		}

		// Send the newly received message to the broadcast channel
		broadcast <- msg
	}
}

func handleMessages() {
	for {
		// Grab the next message from the broadcast channel
		msg := <-broadcast
		// Send it out to every client that is currently connected
		for client := range clients {
			err := client.WriteJSON(msg)
			if err != nil {
				log.Printf("error: %v", err)
				client.Close()
				delete(clients, client)
			}
		}
	}
}

// Main function to start the Snek pipeline
func main() {

	// Start the Snek pipeline
	if err := globalIndexer.Start(); err != nil {
		log.Fatalf("failed to start snek: %s", err)
	}

	// Create a simple file server
	fs := http.FileServer(http.Dir("../public"))
	http.Handle("/", fs)

	// Configure websocket route
	http.HandleFunc("/ws", handleConnections)

	// Start listening for incoming chat messages
	go handleMessages()

	// Start the server on localhost port 8080 and log any errors
	log.Println("http server started on :8080")
	err := http.ListenAndServe("localhost:8080", nil)
	if err != nil {
		log.Fatal("ListenAndServe: ", err)
	}
}
