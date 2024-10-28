package main

import (
	"context"
	"crypto/ecdsa"
	"flag"
	"fmt"
	"log"
	"math/big"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

type EthTPSTester struct {
	client       *ethclient.Client
	privateKey   *ecdsa.PrivateKey
	fromAddress  common.Address
	toAddress    common.Address
	numWorkers   int
	testDuration time.Duration
	successCount uint64
	failureCount uint64
	nonceMutex   sync.Mutex
	currentNonce uint64
	stopChan     chan struct{}
	workerWg     sync.WaitGroup
	resultsChan  chan bool
}

func NewEthTPSTester(rpcURL string, privateKeyHex string, toAddressHex string, workers int, duration time.Duration) (*EthTPSTester, error) {
	client, err := ethclient.Dial(rpcURL)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to Ethereum client: %v", err)
	}

	privateKey, err := crypto.HexToECDSA(privateKeyHex)
	if err != nil {
		return nil, fmt.Errorf("invalid private key: %v", err)
	}

	fromAddress := crypto.PubkeyToAddress(privateKey.PublicKey)
	toAddress := common.HexToAddress(toAddressHex)

	// Get initial nonce
	nonce, err := client.PendingNonceAt(context.Background(), fromAddress)
	if err != nil {
		return nil, fmt.Errorf("failed to get nonce: %v", err)
	}

	return &EthTPSTester{
		client:       client,
		privateKey:   privateKey,
		fromAddress:  fromAddress,
		toAddress:    toAddress,
		numWorkers:   workers,
		testDuration: duration,
		currentNonce: nonce,
		stopChan:     make(chan struct{}),
		resultsChan:  make(chan bool, workers*100),
	}, nil
}

func (t *EthTPSTester) getNonce() uint64 {
	t.nonceMutex.Lock()
	defer t.nonceMutex.Unlock()
	nonce := t.currentNonce
	t.currentNonce++
	return nonce
}

func (t *EthTPSTester) sendTransaction() error {
	nonce := t.getNonce()

	gasPrice, err := t.client.SuggestGasPrice(context.Background())
	if err != nil {
		return fmt.Errorf("failed to get gas price: %v", err)
	}

	chainID, err := t.client.ChainID(context.Background())
	if err != nil {
		return fmt.Errorf("failed to get chain ID: %v", err)
	}

	tx := types.NewTransaction(
		nonce,
		t.toAddress,
		big.NewInt(1),
		21000,
		gasPrice,
		nil,
	)

	signedTx, err := types.SignTx(tx, types.NewEIP155Signer(chainID), t.privateKey)
	if err != nil {
		return fmt.Errorf("failed to sign transaction: %v", err)
	}

	return t.client.SendTransaction(context.Background(), signedTx)
}

func (t *EthTPSTester) worker() {
	defer t.workerWg.Done()

	for {
		select {
		case <-t.stopChan:
			return
		default:
			err := t.sendTransaction()
			t.resultsChan <- (err == nil)
			if err != nil {
				log.Printf("Transaction failed: %v", err)
			}
			// Small delay to prevent overwhelming the network
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func (t *EthTPSTester) resultCollector() {
	for success := range t.resultsChan {
		if success {
			atomic.AddUint64(&t.successCount, 1)
		} else {
			atomic.AddUint64(&t.failureCount, 1)
		}
	}
}

func (t *EthTPSTester) Run() {
	fmt.Printf("Starting Ethereum TPS test\n")
	fmt.Printf("From address: %s\n", t.fromAddress.Hex())
	fmt.Printf("To address: %s\n", t.toAddress.Hex())
	fmt.Printf("Number of workers: %d\n", t.numWorkers)
	fmt.Printf("Test duration: %s\n\n", t.testDuration)

	balance, err := t.client.BalanceAt(context.Background(), t.fromAddress, nil)
	if err != nil {
		log.Printf("Failed to get balance: %v", err)
		return
	}
	fmt.Printf("Starting balance: %s wei\n\n", balance.String())

	startTime := time.Now()

	// Start result collector
	go t.resultCollector()

	// Start workers
	for i := 0; i < t.numWorkers; i++ {
		t.workerWg.Add(1)
		go t.worker()
	}

	// Wait for test duration
	time.Sleep(t.testDuration)

	// Stop all workers
	close(t.stopChan)
	t.workerWg.Wait()
	close(t.resultsChan)

	// Calculate results
	duration := time.Since(startTime)
	totalTx := t.successCount + t.failureCount
	tps := float64(t.successCount) / duration.Seconds()

	fmt.Println("\nTest Results:")
	fmt.Printf("Total Duration: %.2f seconds\n", duration.Seconds())
	fmt.Printf("Total Transactions: %d\n", totalTx)
	fmt.Printf("Successful Transactions: %d\n", t.successCount)
	fmt.Printf("Failed Transactions: %d\n", t.failureCount)
	fmt.Printf("Success Rate: %.2f%%\n", float64(t.successCount)/float64(totalTx)*100)
	fmt.Printf("Transactions Per Second (TPS): %.2f\n", tps)

	// Get final balance
	finalBalance, err := t.client.BalanceAt(context.Background(), t.fromAddress, nil)
	if err != nil {
		log.Printf("Failed to get final balance: %v", err)
		return
	}
	fmt.Printf("\nFinal balance: %s wei\n", finalBalance.String())
	fmt.Printf("Total cost: %s wei\n", new(big.Int).Sub(balance, finalBalance).String())
}

func main() {
	rpcURL := flag.String("rpc", "", "Ethereum RPC URL")
	privateKey := flag.String("key", "", "Private key (without 0x prefix)")
	toAddress := flag.String("to", "", "Recipient address")
	workers := flag.Int("workers", 10, "Number of concurrent workers")
	duration := flag.Duration("duration", 30*time.Second, "Test duration (e.g., 30s, 1m)")
	flag.Parse()

	if *rpcURL == "" || *privateKey == "" || *toAddress == "" {
		fmt.Println("Please provide all required parameters:")
		fmt.Println("  -rpc: Ethereum RPC URL")
		fmt.Println("  -key: Private key (without 0x prefix)")
		fmt.Println("  -to: Recipient address")
		fmt.Println("  -workers: Number of concurrent workers (default: 10)")
		fmt.Println("  -duration: Test duration (default: 30s)")
		return
	}

	tester, err := NewEthTPSTester(*rpcURL, *privateKey, *toAddress, *workers, *duration)
	if err != nil {
		log.Fatalf("Failed to create tester: %v", err)
	}

	tester.Run()
}
