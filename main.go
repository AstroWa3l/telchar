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
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/blinklabs-io/adder/event"
	filter_event "github.com/blinklabs-io/adder/filter/event"
	"github.com/blinklabs-io/adder/input/chainsync"
	output_embedded "github.com/blinklabs-io/adder/output/embedded"
	"github.com/blinklabs-io/adder/pipeline"
	"github.com/btcsuite/btcutil/bech32"
	koios "github.com/cardano-community/koios-go-client/v3"
	"github.com/cenkalti/backoff/v4"
	"github.com/gorilla/websocket"
	"github.com/spf13/viper"
	telebot "gopkg.in/tucnak/telebot.v2"
)

// Define fullBlockSize as a constant
const fullBlockSize = 87.97

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

type Blocks struct {
	CurrentEpoch int
	BlockCount   int
}

// IncrementBlockCount increases the block count by one
func (p *Blocks) IncrementBlockCount() {
	p.BlockCount++
	SaveBlockCount(*p)
}

// ResetBlockCount resets the block count to zero
func (p *Blocks) ResetBlockCount() {
	p.BlockCount = 0
}

// CheckEpoch checks if the given epoch is the current one. If not, it resets the block count and updates the current epoch
func (p *Blocks) CheckEpoch(epoch int) {
	if p.CurrentEpoch != epoch {
		p.CurrentEpoch = epoch
		p.ResetBlockCount()
		SaveBlockCount(*p)
	}
}

const persistenceFile = "block_count.json"

// SaveBlockCount persists the current block count and epoch to a file
func SaveBlockCount(blocks Blocks) {
	data, err := json.Marshal(blocks)
	if err != nil {
		log.Fatalf("Error marshalling block count: %v", err)
	}
	err = os.WriteFile(persistenceFile, data, 0644)
	if err != nil {
		log.Fatalf("Error writing block count to file: %v", err)
	}
}

// LoadBlockCount loads the block count and epoch from a file, or initializes it if the file doesn't exist
func LoadBlockCount() *Blocks {
	var blocks Blocks
	data, err := os.ReadFile(persistenceFile)
	if err != nil {
		if os.IsNotExist(err) {
			// File doesn't exist, so let's initialize it with default Blocks data
			log.Println("block_count.json does not exist, initializing with default data.")
			blocks = Blocks{CurrentEpoch: 0, BlockCount: 0} // Adjust with your actual default values
			SaveBlockCount(blocks)                          // Persist the initial data to file
		} else {
			// Some other error occurred
			log.Fatalf("Error reading block count from file: %v", err)
		}
	} else {
		err = json.Unmarshal(data, &blocks)
		if err != nil {
			log.Fatalf("Error unmarshalling block count: %v", err)
		}
	}
	return &blocks
}

// Indexer struct to manage the adder pipeline and block events
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
	poolName         string
	epoch            int
	networkMagic     int
	wg               sync.WaitGroup
}

// TwitterCredentials represents the Twitter API credentials
type TwitterCredentials struct {
	ConsumerKey       string `json:"consumer_key"`
	ConsumerSecret    string `json:"consumer_secret"`
	AccessToken       string `json:"access_token"`
	AccessTokenSecret string `json:"access_token_secret"`
}

// Singleton instance of the Indexer
var globalIndexer = &Indexer{}

var blockCount = &Blocks{}

// Start the adder pipeline and handle block events
func (i *Indexer) Start() error {

	// Increment the WaitGroup counter
	i.wg.Add(1)

	defer func() {
		// Decrement the WaitGroup counter when the function exits
		i.wg.Done()

		if r := recover(); r != nil {
			log.Println("Recovered in handleEvent", r)
		}
	}()

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
	i.networkMagic = viper.GetInt("networkMagic")

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

	/// Initialize the kois client based on networkMagic number
	if i.networkMagic == 1 {
		i.koios, e = koios.New(
			koios.Host(koios.PreProdHost),
			koios.APIVersion("v1"),
		)
		if e != nil {
			log.Fatal(e)
		}
	} else if i.networkMagic == 2 {
		i.koios, e = koios.New(
			koios.Host(koios.PreviewHost),
			koios.APIVersion("v1"),
		)
		if e != nil {
			log.Fatal(e)
		}
	} else {
		i.koios, e = koios.New(
			koios.Host(koios.MainnetHost),
			koios.APIVersion("v1"),
		)
		if e != nil {
			log.Fatal(e)
		}
	}

	// Get the current epoch
	epochInfo, err := i.koios.GetTip(context.Background(), nil)
	if err != nil {
		log.Fatalf("failed to get epoch info: %s", err)
	}

	i.epoch = int(epochInfo.Data.EpochNo)

	fmt.Println("Epoch: ", epochInfo.Data.EpochNo)

	// Convert the poolId to Bech32
	bech32PoolId, err := convertToBech32(i.poolId)
	if err != nil {
		log.Printf("failed to convert pool id to Bech32: %s", err)
		fmt.Println("PoolId: ", bech32PoolId)
	}

	// Set the bech32PoolId field in the Indexer
	i.bech32PoolId = bech32PoolId

	// Get pool info
	poolInfo, err := i.koios.GetPoolInfo(context.Background(), koios.PoolID(i.bech32PoolId), nil)
	if err != nil {
		log.Fatalf("failed to get pool lifetime blocks: %s", err)
	}

	if poolInfo.Data != nil {
		i.totalBlocks = poolInfo.Data.BlockCount
		fmt.Println("Total Blocks: ", i.totalBlocks)
	} else {
		log.Fatalf("failed to get pool lifetime blocks: %s", err)
	}

	// Get pool ticker
	if poolInfo.Data.MetaJSON.Ticker != nil {
		i.ticker = *poolInfo.Data.MetaJSON.Ticker
	} else {
		i.ticker = ""
	}

	// Get pool name
	if poolInfo.Data.MetaJSON.Name != nil {
		i.poolName = *poolInfo.Data.MetaJSON.Name
	} else {
		i.poolName = ""
	}

	channelID, err := strconv.ParseInt(i.telegramChannel, 10, 64)
	if err != nil {
		log.Fatalf("failed to parse telegram channel ID: %s", err)
	}

	initMessage := fmt.Sprintf("duckBot initiated!\n\n %s\n Epoch: %d\n Lifetime Blocks: %d\n\n Quack Will Robinson, QUACK!",
		i.poolName, epochInfo.Data.EpochNo, i.totalBlocks)

	_, err = i.bot.Send(&telebot.Chat{ID: channelID}, initMessage)
	if err != nil {
		log.Printf("failed to send Telegram message: %s", err)
	}

	const maxRetries = 3

	// Wrap the pipeline start in a function for the backoff operation
	startPipelineFunc := func(host string) error {
		// Use the host to connect to the Cardano node
		node := chainsync.WithAddress(host)
		inputOpts := []chainsync.ChainSyncOptionFunc{
			node,
			chainsync.WithNetworkMagic(uint32(i.networkMagic)),
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

}

// Handle block events received from the adder pipeline
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

	if len(data) == 0 {
		return fmt.Errorf("no data to unmarshal")
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
			localTime := parsedTime.In(time.Local)
			blockEvent.Timestamp = localTime.Format("January 2, 2006 15:04:05 MST")
			fmt.Printf("Local Time: %s\n", blockEvent.Timestamp)
		}

		// Customize links based on the network magic number
		var cexplorerLink string
		if i.networkMagic == 1 {
			cexplorerLink = "https://preprod.cexplorer.io/block/"
		} else if i.networkMagic == 2 {
			cexplorerLink = "https://preview.cexplorer.io/block/"
		} else {
			cexplorerLink = "https://cexplorer.io/block/"
		}

		if blockEvent.Payload.IssuerVkey == i.poolId {

			tipInfo, err := i.koios.GetTip(context.Background(), nil)
			if err != nil {
				log.Fatalf("failed to get epoch info: %s", err)
			}

			// Current epoch
			epoch := int(tipInfo.Data.EpochNo)

			blockCount.CheckEpoch(epoch)

			blockCount.IncrementBlockCount()

			// // Getting the current epoch's blocks made by a specific pool
			// currentEpochBlocks, err := i.koios.GetPoolBlocks(context.Background(), koios.PoolID(i.bech32PoolId), &epoch, nil)
			// if err != nil {
			// 	log.Fatal(err)
			// }
			// We need to count the length of the json array in order to get the number of blocks
			// currBlocks := len(currentEpochBlocks.Data)

			blockSizeKB := float64(blockEvent.Payload.BlockBodySize) / 1024
			sizePercentage := (blockSizeKB / fullBlockSize) * 100

			msg := fmt.Sprintf(
				"Quack!(attention) 🦆\nduckBot notification!\n\n"+"%s\n"+"💥 New Block!\n\n"+
					"Tx Count: %d\n"+
					"Block Size: %.2f KB\n"+
					"%.2f%% Full\n\n"+
					"Epoch %d\n"+
					"Blocks: %d\n"+
					"Lifetime Blocks: %d\n\n"+
					"Pooltool: https://pooltool.io/realtime/%d\n\n"+
					"Cexplorer: "+cexplorerLink+"%s",
				i.ticker, blockEvent.Payload.TransactionCount, blockSizeKB, sizePercentage,
				epoch, blockCount.BlockCount, i.totalBlocks,
				blockEvent.Context.BlockNumber, blockEvent.Payload.BlockHash)

			photo := &telebot.Photo{File: telebot.FromURL(getDuckImage()), Caption: msg}
			_, err = i.bot.Send(channel, photo)
			if err != nil {
				log.Printf("failed to send Telegram message: %s", err)
			}
		}

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

	// Start error handler in a goroutine
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

// Function to make a request to random duck image API
func getDuckImage() string {
	resp, err := http.Get("https://random-d.uk/api/v2/random")
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)

	imageURL := result["url"].(string)
	return imageURL
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

// Main function to start the adder pipeline
func main() {

	blockCount = LoadBlockCount()

	// Start the adder pipeline
	if err := globalIndexer.Start(); err != nil {
		log.Fatalf("failed to start adder: %s", err)
	}

	// Configure websocket route
	http.HandleFunc("/ws", handleConnections)

	// Start listening for incoming chat messages
	go handleMessages()

	// Wait for all goroutines to finish before exiting
	globalIndexer.wg.Wait()

	// Start the server on localhost port 8080 and log any errors
	log.Println("http server started on :8080")
	err := http.ListenAndServe("localhost:8080", nil)
	if err != nil {
		log.Fatal("ListenAndServe: ", err)
	}
}
