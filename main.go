package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/blinklabs-io/snek/event"
	filter_event "github.com/blinklabs-io/snek/filter/event"
	"github.com/blinklabs-io/snek/input/chainsync"
	output_embedded "github.com/blinklabs-io/snek/output/embedded"
	"github.com/blinklabs-io/snek/pipeline"
	"github.com/btcsuite/btcutil/bech32"
	koios "github.com/cardano-community/koios-go-client/v3"
	"github.com/cenkalti/backoff/v4"
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
	poolId           string
	telegramChannel  string
	telegramToken    string
	image            string
	ticker           string
	koios            *koios.Client
	bech32PoolId     string
	nodeAddresses    []string
	totalBlocks      uint64
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

	// Set the configuration values
	i.poolId = viper.GetString("poolId")
	i.telegramChannel = viper.GetString("telegram.channel")
	i.telegramToken = viper.GetString("telegram.token")
	i.image = viper.GetString("image")

	// Store the node addresses hosts into the array nodeAddresses in the Indexer
	i.nodeAddresses = viper.GetStringSlice("nodeAddress.host1")
	i.nodeAddresses = append(i.nodeAddresses, viper.GetStringSlice("nodeAddress.host2")...)

	// Initialize the bot
	var err error
	i.bot, err = telebot.NewBot(telebot.Settings{
		Token:  i.telegramToken,
		Poller: &telebot.LongPoller{Timeout: 10 * time.Second},
	})

	if err != nil {
		log.Fatalf("failed to start bot: %s", err)
	}

	/// Initialize the kois client
	i.koios, e = koios.New()
	if e != nil {
		log.Fatal(e)
	}

	// Convert the poolId to Bech32
	bech32PoolId, err := convertToBech32(i.poolId)
	if err != nil {
		log.Printf("failed to convert pool id to Bech32: %s", err)
		fmt.Println("PoolId: ", bech32PoolId)
	}

	i.bech32PoolId = bech32PoolId

	// Get pool lifetime blocks
	poolInfo, err := i.koios.GetPoolInfo(context.Background(), koios.PoolID(i.bech32PoolId), nil)
	if err != nil {
		log.Fatalf("failed to get pool lifetime blocks: %s", err)
	}

	i.totalBlocks = poolInfo.Data.BlockCount
	fmt.Println("Total Blocks: ", i.totalBlocks)

	const maxRetries = 3

	// Wrap the pipeline start in a function for the backoff operation
	startPipelineFunc := func(host string) error {
		// Use the host to connect to the Cardano node
		node := chainsync.WithAddress(host)
		inputOpts := []chainsync.ChainSyncOptionFunc{
			node,
			chainsync.WithNetworkMagic(764824073),
			chainsync.WithIntersectTip(true),
			chainsync.WithAutoReconnect(true),
		}

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

		err := i.pipeline.Start()
		if err != nil {
			log.Printf("Failed to start pipeline: %s. Retrying...", err)
			return err
		}
		return nil
	}

	// Initialize the backoff strategy
	bo := backoff.NewExponentialBackOff()
	bo.MaxElapsedTime = time.Minute // Max duration to keep retrying

	hosts := i.nodeAddresses
	for _, host := range hosts {
		for i := 0; i < maxRetries; i++ {
			err := startPipelineFunc(host)
			if err == nil {
				return nil
			}
			time.Sleep(time.Duration(i) * time.Second)
		}
	}

	log.Fatalf("Failed to start pipeline after several attempts")
	return errors.New("Failed to start pipeline after several attempts")

	////////////////////////////////////////OLD CODE////////////////////////////////////////
	// // Initialize the backoff strategy
	// bo := backoff.NewExponentialBackOff()
	// bo.MaxElapsedTime = time.Minute // Max duration to keep retrying

	// // Wrap the pipeline start in a function for the backoff operation
	// startPipelineFunc := func() error {
	// 	err := i.pipeline.Start()
	// 	if err != nil {
	// 		log.Printf("Failed to start pipeline: %s. Retrying...", err)
	// 		return err
	// 	}
	// 	return nil
	// }

	// // Use the backoff strategy in the pipeline start
	// if err := backoff.Retry(startPipelineFunc, bo); err != nil {
	// 	log.Fatalf("Failed to start pipeline after several attempts: %s", err)
	// 	return err
	// }

	// // Start error handler in a goroutine
	// go func() {
	// 	for {
	// 		err, ok := <-i.pipeline.ErrorChan()
	// 		if ok {
	// 			log.Printf("pipeline failed: %s\n", err)
	// 			log.Println("Restarting pipeline...")
	// 			if err := i.pipeline.Start(); err != nil {
	// 				log.Fatalf("failed to restart pipeline: %s\n", err)
	// 			}
	// 		}
	// 	}
	// }()

	// return nil
	////////////////////////////////////////OLD CODE////////////////////////////////////////
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
		return fmt.Errorf("error unmarshalling event: %v", err)
	}

	// Unmarshal the event to get the event type
	var getEvent map[string]interface{}
	errr := json.Unmarshal(data, &getEvent)
	if errr != nil {
		return fmt.Errorf("error unmarshalling event: %v", err)
	}

	eventType, ok := getEvent["type"].(string)
	if !ok {
		return fmt.Errorf("failed to get event type")
	}

	channel, err := i.bot.ChatByID(i.telegramChannel)
	if err != nil {
		panic(err) // You should add better error handling than this!
	}

	switch eventType {
	case "chainsync.block":
		var blockEvent BlockEvent
		err := json.Unmarshal(data, &blockEvent)
		if err != nil {
			return fmt.Errorf("error unmarshalling block event: %v", err)
		}

		bech32IssuerVkey, err := convertToBech32(blockEvent.Payload.IssuerVkey)
		if err != nil {
			log.Printf("failed to convert issuer vkey to Bech32: %s", err)
		} else {
			fmt.Printf("IssuerVkey: %s\n", bech32IssuerVkey)
		}

		parsedTime, err := time.Parse(time.RFC3339, blockEvent.Timestamp)
		if err == nil {
			blockEvent.Timestamp = parsedTime.Format("January 2, 2006 15:04:05 MST")
			fmt.Printf("Time: %s\n", blockEvent.Timestamp)
		}

		if blockEvent.Payload.IssuerVkey == i.poolId {
			msg := fmt.Sprintf("New Block Forged 🧱: https://pooltool.io/realtime/%d\n\nHash#️⃣: https://cexplorer.io/block/%s\n\nTx Count🔢: %d\n\n🕰: %s\n\nTotal 🧱's Forged: %d",
				blockEvent.Context.BlockNumber, blockEvent.Payload.BlockHash, blockEvent.Payload.TransactionCount, blockEvent.Timestamp, i.totalBlocks)
			photo := &telebot.Photo{File: telebot.FromURL(i.image), Caption: msg}
			_, err = i.bot.Send(channel, photo)
			if err != nil {
				log.Printf("failed to send Telegram message: %s", err)
			}
		}
		// } else {
		// 	msg := fmt.Sprintf("New %s Block Forged 🧱: https://pooltool.io/realtime/%d\n\nHash#️⃣: https://cexplorer.io/block/%s\n\nTx Count🔢: %d\n\n🕰: %s",
		// 		i.ticker, blockEvent.Context.BlockNumber, blockEvent.Payload.BlockHash,
		// 		blockEvent.Payload.TransactionCount, blockEvent.Timestamp)
		// 	// Get random duck image and send it to the Telegram channel with the message msg as caption
		// 	photo := &telebot.Photo{File: telebot.FromURL(i.image), Caption: msg}
		// 	_, err = i.bot.Send(channel, photo)
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
			return fmt.Errorf("error unmarshalling rollback event: %v", err)
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

	go func() {
		for {
			err, ok := <-i.pipeline.ErrorChan()
			if ok {
				if errors.Is(err, io.EOF) {
					log.Println("Input source is exhausted, stopping pipeline...")
					i.pipeline.Stop()
					time.Sleep(time.Second * 10) // wait for 10 seconds
					log.Println("Restarting pipeline...")
					if err := i.pipeline.Start(); err != nil {
						log.Fatalf("failed to restart pipeline: %s\n", err)
					}
				} else {
					log.Printf("pipeline failed: %s\n", err)
					log.Println("Restarting pipeline...")
					if err := i.pipeline.Start(); err != nil {
						log.Fatalf("failed to restart pipeline: %s\n", err)
					}
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

func convertToBech32(hash string) (string, error) {
	bytes, err := hex.DecodeString(hash)
	if err != nil {
		return "", err
	}

	fiveBitWords, err := bech32.ConvertBits(bytes, 8, 5, true)
	if err != nil {
		return "", err
	}

	bech32Str, err := bech32.Encode("pool", fiveBitWords)
	if err != nil {
		return "", err
	}

	return bech32Str, nil
}

// Main function to start the Snek pipeline
func main() {

	// testing koios
	api, e := koios.New()
	if e != nil {
		log.Fatal(e)
	}

	res, er := api.GetTip(context.Background(), nil)
	if er != nil {
		log.Fatal(er)
	}
	fmt.Println("status: ", res.Status)
	fmt.Println("statu_code: ", res.StatusCode)

	fmt.Println("Epoch: ", res.Data.EpochNo)

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
	log.Println("http server started on :8008")
	err := http.ListenAndServe("localhost:8008", nil)
	if err != nil {
		log.Fatal("ListenAndServe: ", err)
	}
}
