package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	//	"io"
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
const persistenceFile = "block_count.json"
const (
	EpochDurationInDays = 5
	ShelleyStartSlot    = 4492800
	SecondsInDay        = 24 * 60 * 60
	ShelleyEpochStart   = "2020-07-29T21:44:51Z"
	StartingEpoch       = 208
)

// Singleton instance of the Indexer
var globalIndexer = &Indexer{}
var blockCount = &Blocks{}

// Channel to broadcast block events to connected clients
var clients = make(map[*websocket.Conn]bool) // connected clients
var broadcast = make(chan interface{})       // broadcast channel

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

// Indexer struct to manage the adder pipeline and block events
type Indexer struct {
	pipeline        *pipeline.Pipeline
	bot             *telebot.Bot
	poolId          string
	telegramChannel string
	telegramToken   string
	image           string
	ticker          string
	koios           *koios.Client
	bech32PoolId    string
	epochBlocks     int
	nodeAddresses   []string
	totalBlocks     uint64
	poolName        string
	epoch           int
	networkMagic    int
	wg              sync.WaitGroup
}

type BlockEvent struct {
	Type      string                 `json:"type"`
	Timestamp string                 `json:"timestamp"`
	Context   chainsync.BlockContext `json:"context"`
	Payload   chainsync.BlockEvent   `json:"payload"`
}

type Blocks struct {
	CurrentEpoch int
	BlockCount   int
}

// Function to calculate the current epoch number
func getCurrentEpoch() int {
	// Parse the Shelley epoch start time
	shelleyStartTime, err := time.Parse(time.RFC3339, ShelleyEpochStart)
	if err != nil {
		fmt.Println("Error parsing Shelley start time:", err)
		return -1
	}
	// Calculate the elapsed time since Shelley start in seconds
	elapsedSeconds := time.Since(shelleyStartTime).Seconds()
	// Calculate the number of epochs elapsed
	epochsElapsed := int(elapsedSeconds / (EpochDurationInDays * SecondsInDay))
	// Calculate the current epoch number
	currentEpoch := StartingEpoch + epochsElapsed

	return currentEpoch
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
	i.ticker = viper.GetString("ticker")
	i.poolName = viper.GetString("poolName")
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

	i.epoch = getCurrentEpoch()
	fmt.Println("Epoch: ", i.epoch)

	// Convert the poolId to Bech32
	bech32PoolId, err := convertToBech32(i.poolId)
	if err != nil {
		log.Printf("failed to convert pool id to Bech32: %s", err)
		fmt.Println("PoolId: ", bech32PoolId)
	}

	// Set the bech32PoolId field in the Indexer
	i.bech32PoolId = bech32PoolId
	// Get lifetime blocks
	lifetimeBlocks, err := i.koios.GetPoolInfo(context.Background(), koios.PoolID(i.bech32PoolId), nil)
	if err != nil {
		log.Fatalf("failed to get pool lifetime blocks: %s", err)
	}

	// Set the bech32PoolId field in the Indexer
	i.bech32PoolId = bech32PoolId
	// Get epoch blocks
	epoch := koios.EpochNo(i.epoch)
	epochBlocks, err := i.koios.GetPoolBlocks(context.Background(), koios.PoolID(i.bech32PoolId), &epoch, nil)

	if epochBlocks.Data != nil {
		i.epochBlocks = len(epochBlocks.Data)
		fmt.Println("Epoch Blocks: ", i.epochBlocks)
	} else {
		log.Fatalf("failed to get pool lifetime blocks: %s", err)
	}
	
	if lifetimeBlocks.Data != nil {
		i.totalBlocks = lifetimeBlocks.Data.BlockCount
		fmt.Println("Total Blocks: ", i.totalBlocks)
	} else {
		log.Fatalf("failed to get pool lifetime blocks: %s", err)
	}
	//	// Get pool ticker
	//	if poolInfo.Data.MetaJSON.Ticker != nil {
	//		i.ticker = *poolInfo.Data.MetaJSON.Ticker
	//	} else {
	//		i.ticker = ""
	//	}
	// Get pool name
	//	if poolInfo.Data.MetaJSON.Name != nil {
	//		i.poolName = *poolInfo.Data.MetaJSON.Name
	//	} else {
	//		i.poolName = ""
	//	}
	channelID, err := strconv.ParseInt(i.telegramChannel, 10, 64)
	if err != nil {
		log.Fatalf("failed to parse telegram channel ID: %s", err)
	}
	initMessage := fmt.Sprintf("duckBot initiated!\n\n %s\n Epoch: %d\n Lifetime Blocks: %d\n\n Quack Will Robinson, QUACK!",
		i.poolName, i.epoch, i.totalBlocks)

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
		filterEvent := filter_event.New(filter_event.WithTypes([]string{"chainsync.block"}))
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
	// Marshal the event to JSON
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("error marshalling event: %v", err)
	}

	// Unmarshal the event to get the block event details
	var blockEvent BlockEvent
	err = json.Unmarshal(data, &blockEvent)
	if err != nil {
		return fmt.Errorf("error unmarshalling block event: %v", err)
	}

	// Convert the block event timestamp to local time
	parsedTime, err := time.Parse(time.RFC3339, blockEvent.Timestamp)
	if err == nil {
		localTime := parsedTime.In(time.Local)
		blockEvent.Timestamp = localTime.Format("January 2, 2006 15:04:05 MST")
		fmt.Printf("Local Time: %s\n", blockEvent.Timestamp)
	}

	// Convert issuer Vkey to Bech32
	bech32IssuerVkey, err := convertToBech32(blockEvent.Payload.IssuerVkey)
	if err != nil {
		log.Printf("failed to convert issuer vkey to Bech32: %s", err)
	} else {
		fmt.Printf("IssuerVkey: %s\n", bech32IssuerVkey)
	}

	// Customize links based on the network magic number
	var cexplorerLink string
	switch i.networkMagic {
	case 1:
		cexplorerLink = "https://preprod.cexplorer.io/block/"
	case 2:
		cexplorerLink = "https://preview.cexplorer.io/block/"
	default:
		cexplorerLink = "https://cexplorer.io/block/"
	}

	// If the block event is from the pool, process it
	if blockEvent.Payload.IssuerVkey == i.poolId {
		//        tipInfo, err := i.koios.GetTip(context.Background(), nil)
		//        if err != nil {
		//            log.Fatalf("failed to get epoch info: %s", err)
		//        }

		// Current epoch
		//       epoch := i.epoch

		blockCount.CheckEpoch(i.epoch)
		blockCount.IncrementBlockCount()

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
			i.poolName, blockEvent.Payload.TransactionCount, blockSizeKB, sizePercentage,
			i.epoch, blockCount.BlockCount, i.totalBlocks,
			blockEvent.Context.BlockNumber, blockEvent.Payload.BlockHash)

		// Send the message to the appropriate channel
		channelID, err := strconv.ParseInt(i.telegramChannel, 10, 64)
		if err != nil {
			log.Fatalf("failed to parse telegram channel ID: %s", err)
		}
		photo := &telebot.Photo{File: telebot.FromURL(getDuckImage()), Caption: msg}
		_, err = i.bot.Send(&telebot.Chat{ID: channelID}, photo)
		if err != nil {
			log.Printf("failed to send Telegram message: %s", err)
		}
	}

	// Print the received event information
	fmt.Printf("Received Event: %+v\n", blockEvent)

	// Send the block event to the WebSocket clients
	broadcast <- blockEvent

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

// Handle broadcasting the messages to clients
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

// Get random duck
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

// Convert to bech32 poolID
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
