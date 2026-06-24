package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"log/slog"

	"github.com/Infrared-Trading-Technologies/go-gas/pkg/estimator"
	"github.com/Infrared-Trading-Technologies/go-gas/pkg/eth"
)

func main() {
	// 1. Configuration
	httpURL := os.Getenv("GAS_NODE_HTTP_URL")
	wsURL := os.Getenv("GAS_NODE_WS_URL")

	if httpURL == "" || wsURL == "" {
		log.Fatal("Please set GAS_NODE_HTTP_URL and GAS_NODE_WS_URL environment variables")
	}

	// 2. Setup dependencies
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	// HTTP Client for fetching historical blocks
	client := eth.NewClient(httpURL)
	defer client.Close()

	// WebSocket Subscriber for real-time updates
	sub := eth.NewWSSubscriber(wsURL, logger)
	defer sub.Close()

	// Provider stores the latest estimate atomically
	provider := estimator.NewProvider()

	// 3. Initialize Estimator
	est := estimator.New(
		client, // BlockReader
		client, // TransactionReader
		sub,    // Subscriber
		provider,
		estimator.WithHistorySize(20),
		estimator.WithMempoolSamples(200),
		estimator.WithRecalcInterval(1*time.Second),
		estimator.WithLogger(logger),
	)

	// 4. Run Estimator in background
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	go func() {
		logger.Info("Starting estimator...")
		if err := est.Run(ctx); err != nil {
			if err != context.Canceled {
				logger.Error("Estimator failed", "error", err)
				os.Exit(1)
			}
		}
	}()

	// 5. Consume estimates
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	fmt.Println("Waiting for first estimate...")

	for {
		select {
		case <-ctx.Done():
			fmt.Println("\nShutting down...")
			return
		case <-ticker.C:
			// Get the latest estimate (thread-safe, lock-free)
			estimate, err := provider.Current(ctx)
			if err == estimator.ErrNotReady {
				continue
			}
			if err != nil {
				logger.Error("Failed to get estimate", "error", err)
				continue
			}

			// Print the estimate
			fmt.Printf("\n--- Gas Estimate (Block %d) ---\n", estimate.BlockNumber)
			fmt.Printf("Base Fee: %s wei\n", estimate.BaseFee.Dec())
			fmt.Printf("Urgent (99%%): %s wei (Total: %s)\n",
				estimate.Urgent.MaxPriorityFeePerGas.Dec(),
				estimate.Urgent.MaxFeePerGas.Dec())
			fmt.Printf("Fast   (90%%): %s wei\n", estimate.Fast.MaxPriorityFeePerGas.Dec())
			fmt.Printf("Std    (50%%): %s wei\n", estimate.Standard.MaxPriorityFeePerGas.Dec())
			fmt.Printf("Slow   (25%%): %s wei\n", estimate.Slow.MaxPriorityFeePerGas.Dec())
		}
	}
}
