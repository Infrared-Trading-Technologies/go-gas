# Gas Estimator Service

High-performance, ultra low-latency Ethereum gas price estimator for Go.

`go-gas` is designed for high-frequency trading, MEV bots, and real-time applications where millisecond-latency matters. It uses a push-based architecture and highly optimized arithmetic (via `uint256`) to deliver gas estimates with minimal heap allocations.

[![Go Reference](https://pkg.go.dev/badge/github.com/Infrared-Trading-Technologies/go-gas.svg)](https://pkg.go.dev/github.com/Infrared-Trading-Technologies/go-gas)
[![Go Report Card](https://goreportcard.com/badge/github.com/Infrared-Trading-Technologies/go-gas)](https://goreportcard.com/report/github.com/Infrared-Trading-Technologies/go-gas)

## Features

- **Ultra Low Latency**: ~69µs calculation time per update.
- **Zero-Copy Math**: Built on `github.com/holiman/uint256` to avoid `math/big` GC pressure.
- **Push-Based**: Subscribes to WebSocket block headers and pending transactions.
- **Hybrid Strategy**: Combines historical block analysis (EIP-1559) with real-time mempool sampling.
- **Thread Safe**: Lock-free reads via atomic pointer swapping.
- **Flexible**: Run as a standalone gRPC/HTTP service or import as a Go library.

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                    Gas Estimator Service                     │
├─────────────────────────────────────────────────────────────┤
│                                                              │
│   ┌──────────┐     ┌───────────────┐     ┌──────────────┐   │
│   │  Node    │────▶│   Estimator   │────▶│   Provider   │   │
│   │ (WS+RPC) │     │ (Orchestrator)│     │ (atomic.Ptr) │   │
│   └──────────┘     └───────────────┘     └──────┬───────┘   │
│                           │                      │           │
│                    ┌──────┴──────┐               │           │
│                    │             │               ▼           │
│               ┌────┴────┐  ┌─────┴─────┐  ┌───────────┐     │
│               │ History │  │ Calculator│  │ API Server│     │
│               │ (Ring)  │  │ (Strategy)│  │  (HTTP)   │     │
│               └─────────┘  └───────────┘  └───────────┘     │
│                                                              │
└─────────────────────────────────────────────────────────────┘
```

## Benchmarks

Performance is the primary goal of this library. We migrated from `math/big` to `uint256` to reduce garbage collection overhead on the hot path.

See [BENCHMARKS.md](BENCHMARKS.md) for the latest results and a detailed history of performance improvements.

## Installation

```bash
go get github.com/Infrared-Trading-Technologies/go-gas
```

## Usage

### As a Library

Embed the estimator directly into your Go application for the lowest possible latency (no network hop).

```go
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
```

### As a Standalone Service

You can also run `go-gas` as a standalone microservice that exposes estimates via gRPC or HTTP.

#### 1. Configuration
Configure via environment variables:

| Variable            | Description                          | Default                 |
| :------------------ | :----------------------------------- | :---------------------- |
| `GAS_NODE_HTTP_URL` | Ethereum Node HTTP URL               | `http://localhost:8545` |
| `GAS_NODE_WS_URL`   | Ethereum Node WebSocket URL          | `ws://localhost:8546`   |
| `GAS_PORT`          | Service Port                         | `8080`                  |
| `GAS_LOG_LEVEL`     | Log Level (debug, info, warn, error) | `info`                  |

#### 2. Run with Docker

```bash
docker run -d \
  -e GAS_NODE_HTTP_URL=https://eth-mainnet.alchemyapi.io/v2/YOUR_KEY \
  -e GAS_NODE_WS_URL=wss://eth-mainnet.alchemyapi.io/v2/YOUR_KEY \
  -p 8080:8080 \
  ghcr.io/infrared-trading-technologies/go-gas:latest
```

#### 3. Run from Source

```bash
# Clone and build
git clone https://github.com/Infrared-Trading-Technologies/go-gas.git
cd go-gas
go build -o gas-estimator cmd/estimator/main.go

# Run
export GAS_NODE_HTTP_URL=...
./gas-estimator
```

## Future Optimizations

To further reduce `chain_lag_ms` and improve responsiveness, the following optimizations are planned:

### Infrastructure
- **Co-location**: Run the estimator on the same physical machine or VPC as the Ethereum node to minimize network latency.
- **IPC over HTTP**: Switch from HTTP to Unix Domain Sockets (IPC) for block fetching when co-located.
- **Node Tuning**: Utilize high-performance clients (Reth/Erigon) on NVMe hardware to speed up `eth_getBlockByNumber` calls.

### Architecture
- **Eliminate Round Trip**: Subscribe to full blocks directly (where supported) instead of the current "Header -> Fetch Body" pattern.
- **Optimistic Updates**: Immediately update the `BaseFee` component of estimates upon receiving a new header, while asynchronously fetching transaction data for `PriorityFee` updates.
- **Streaming Transactions**: Reduce reliance on full block analysis by weighting local mempool data more heavily, allowing for faster (albeit slightly less accurate) updates before the full block body is downloaded.

## Contributing

Pull requests are welcome. For major changes, please open an issue first to discuss what you would like to change.

Please make sure to update tests and run benchmarks as appropriate.

## License

[MIT](https://choosealicense.com/licenses/mit/)
