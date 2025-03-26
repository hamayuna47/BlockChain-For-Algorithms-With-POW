package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"

	shell "github.com/ipfs/go-ipfs-api"
)

type Transaction struct {
	ID   string
	Data string
}

type Block struct {
	PrevHash     string
	Transactions []Transaction
	Nonce        int
	Hash         string
	PrevCID      string
	BlockNumber  int
}

var (
	transactionBuffer = make(chan Transaction, 100) // Buffer for dynamically created transactions
	newBlock          = make(chan Block)           // Channel to broadcast new blocks
	stopMining        = make(chan struct{})        // Channel to stop the mining process
	target            = big.NewInt(1).Lsh(big.NewInt(1), 245) // Approximate target for ~30 seconds
	ipfsShell         = shell.NewShell("localhost:5001")      // IPFS shell instance
	connectedMiners   = []string{}                           // List of connected miner IPs
	minedBlocks       = 0                                    // Number of blocks mined by this node
	blockValidations  = make(map[string]int)                // Track block validation votes (by block hash)
)

// Download file from IPFS.
func downloadFromIPFS(cid, outputPath string) error {
	reader, err := ipfsShell.Cat(cid)
	if err != nil {
		return fmt.Errorf("failed to fetch file from IPFS: %v", err)
	}
	defer reader.Close()

	file, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("failed to create output file: %v", err)
	}
	defer file.Close()

	_, err = io.Copy(file, reader)
	if err != nil {
		return fmt.Errorf("failed to write to output file: %v", err)
	}
	return nil
}

// Execute Python script with input data.
func executeScript(scriptPath, dataPath string) (string, error) {
	cmd := exec.Command("python", scriptPath, dataPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("script execution failed: %v, output: %s", err, string(output))
	}
	return string(output), nil
}

// Transaction Processing Thread
func processTransactions(wg *sync.WaitGroup) {
	defer wg.Done()

	ln, err := net.Listen("tcp", ":8080")
	if err != nil {
		fmt.Println("Error starting transaction listener:", err)
		return
	}
	defer ln.Close()

	for {
		conn, err := ln.Accept()
		if err != nil {
			fmt.Println("Error accepting connection:", err)
			continue
		}

		go func(conn net.Conn) {
			defer conn.Close()

			scanner := bufio.NewScanner(conn)
			for scanner.Scan() {
				message := scanner.Text()
				fmt.Println("Received hashes:", message)

				parts := strings.Split(message, " ")
				if len(parts) != 2 {
					fmt.Println("Invalid message format. Expected '<data_hash> <script_hash>'")
					continue
				}
				dataHash, scriptHash := parts[1], parts[0]

				// Download data and script from IPFS
				dataPath := "data.txt"
				scriptPath := "script.py"

				if err := downloadFromIPFS(dataHash, dataPath); err != nil {
					fmt.Println("Failed to download data:", err)
					continue
				}

				if err := downloadFromIPFS(scriptHash, scriptPath); err != nil {
					fmt.Println("Failed to download script:", err)
					continue
				}

				// Execute the script to produce the transaction
				result, err := executeScript(scriptPath, dataPath)
				if err != nil {
					fmt.Println("Error executing script:", err)
					continue
				}

				// Create a transaction from the result
				transaction := Transaction{
					ID:   generateTransactionID(result),
					Data: result,
				}

				// Add the transaction to the buffer
				transactionBuffer <- transaction
				fmt.Println("Transaction created and added to buffer:", transaction)
			}
		}(conn)
	}
}

// Mining Thread
func startMining(prevHash, prevCID string, wg *sync.WaitGroup) {
	defer wg.Done()

	for {
		select {
		case <-stopMining:
			fmt.Println("Stopping mining thread...")
			return
		default:
			// Wait for exactly 3 transactions
			transactions := make([]Transaction, 0, 3)
			for len(transactions) < 3 {
				tx := <-transactionBuffer // This blocks until a transaction is available
				transactions = append(transactions, tx)
				fmt.Println("Added transaction to block:", tx)
			}

			// Perform proof of work
			nonce := 0
			for {
				select {
				case <-stopMining:
					return
				default:
					blockData := fmt.Sprintf("%s:%v:%d", prevHash, transactions, nonce)
					hash := sha256.Sum256([]byte(blockData))
					hashInt := new(big.Int).SetBytes(hash[:])
					if hashInt.Cmp(target) == -1 {
						block := Block{
							PrevHash:     prevHash,
							Transactions: transactions,
							Nonce:        nonce,
							Hash:         hex.EncodeToString(hash[:]),
							PrevCID:      prevCID,
							BlockNumber:  minedBlocks,
						}
						fmt.Println("Mined a new block:", block.Hash)

						// Update mined blocks count
						minedBlocks++

						// Upload block to IPFS and get its CID
						blockCID, err := uploadBlockToIPFS(block)
						if err != nil {
							fmt.Println("Error uploading block to IPFS:", err)
							continue
						}
						block.PrevCID = blockCID

						// Broadcast the new block to connected miners
						for _, miner := range connectedMiners {
							sendBlockToMiner(miner, block)
						}

						// Add block to the newBlock channel
						newBlock <- block
						return
					}
					nonce++
				}
			}
		}
	}
}

// Broadcast block to other miners
func sendBlockToMiner(miner string, block Block) {
	conn, err := net.Dial("tcp", miner+":8081")
	if err != nil {
		fmt.Println("Error connecting to miner:", err)
		return
	}
	defer conn.Close()

	// Serialize block to JSON
	blockData, err := json.Marshal(block)
	if err != nil {
		fmt.Println("Error serializing block to JSON:", err)
		return
	}

	// Send serialized block data
	_, err = conn.Write(blockData)
	if err != nil {
		fmt.Println("Error sending block to miner:", err)
	}
}


// Upload block to IPFS and return its CID
func uploadBlockToIPFS(block Block) (string, error) {
	blockData := fmt.Sprintf("%v", block)

	// Add the block to IPFS
	cid, err := ipfsShell.Add(strings.NewReader(blockData))
	if err != nil {
		return "", fmt.Errorf("failed to upload block to IPFS: %v", err)
	}

	return cid, nil
}

// Block Reception and Validation Thread
func receiveAndValidateBlocks(wg *sync.WaitGroup) {
	defer wg.Done()

	ln, err := net.Listen("tcp", ":8081")
	if err != nil {
		fmt.Println("Error starting block listener:", err)
		return
	}
	defer ln.Close()

	for {
		conn, err := ln.Accept()
		if err != nil {
			fmt.Println("Error accepting block connection:", err)
			continue
		}

		go func(conn net.Conn) {
			defer conn.Close()

			scanner := bufio.NewScanner(conn)
			for scanner.Scan() {
				blockData := scanner.Text()
				fmt.Println("Received block:", blockData)

				// Deserialize block data into Block struct
				var block Block
				err := json.Unmarshal([]byte(blockData), &block)
				if err != nil {
					fmt.Println("Error decoding block data:", err)
					continue
				}

				// Validate the block
				if validateBlock(blockData, "-1", target) {
					blockHash := getBlockHash(blockData)
					blockValidations[blockHash]++
					if blockValidations[blockHash] > len(connectedMiners)/2 {
						fmt.Println("Block validated and added to blockchain.")
					}
				}
			}
		}(conn)
	}
}


// Validate a block
func validateBlock(blockData string, prevHash string, target *big.Int) bool {
	var block Block
	err := json.Unmarshal([]byte(blockData), &block)
	if err != nil {
		fmt.Println("Error decoding block data:", err)
		return false
	}

	// Check previous hash
	if prevHash != "-1" && block.PrevHash != prevHash {
		fmt.Printf("Invalid block: Previous hash mismatch.\n")
		return false
	}

	// Validate transactions
	for _, tx := range block.Transactions {
		if tx.ID == "" {
			return false
		}
	}
	return true
}
func generateTransactionID(data string) string {
	hash := sha256.Sum256([]byte(data))
	return hex.EncodeToString(hash[:])
}

func getBlockHash(blockData string) string {
	hash := sha256.Sum256([]byte(blockData))
	return hex.EncodeToString(hash[:])
}
// Main function
func main() {
	// WaitGroup for managing goroutines
	var wg sync.WaitGroup

	// Initialize variables for genesis block
	prevHash := "-1" // Placeholder for genesis block
	prevCID := "-1"  // Placeholder for genesis block CID

	// Add goroutines to process transactions
	wg.Add(1)
	go processTransactions(&wg)

	// Add goroutines to receive and validate blocks
	wg.Add(1)
	go receiveAndValidateBlocks(&wg)

	// Simulate some connected miners (replace with real IPs in a network)
	connectedMiners = append(connectedMiners, "127.0.0.1") // Example miner IP

	// Start mining process
	wg.Add(1)
	go startMining(prevHash, prevCID, &wg)

	// Wait for all goroutines to finish
	wg.Wait()
}