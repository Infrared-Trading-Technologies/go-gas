package estimator

import (
	"context"
	"testing"

	"github.com/Infrared-Trading-Technologies/go-gas/pkg/eth"
	"github.com/holiman/uint256"
)

// BenchmarkLocalTxPool_Add measures the cost of ingesting a transaction.
// This happens on the hot path of the WebSocket reader.
func BenchmarkLocalTxPool_Add(b *testing.B) {
	pool := NewLocalTxPool(5000)
	tx := &eth.Transaction{
		Hash:                 "0x123",
		MaxPriorityFeePerGas: uint256.NewInt(1000000000),
		MaxFeePerGas:         uint256.NewInt(2000000000),
		Type:                 2,
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		pool.Add(tx)
	}
}

// BenchmarkLocalTxPool_Snapshot measures the cost of reading the pool.
// This happens every recalculation interval.
func BenchmarkLocalTxPool_Snapshot(b *testing.B) {
	pool := NewLocalTxPool(5000)
	tx := &eth.Transaction{
		Hash:                 "0x123",
		MaxPriorityFeePerGas: uint256.NewInt(1000000000),
		MaxFeePerGas:         uint256.NewInt(2000000000),
		Type:                 2,
	}
	// Fill pool
	for i := 0; i < 5000; i++ {
		pool.Add(tx)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = pool.Snapshot()
	}
}

// BenchmarkStrategy_Calculate measures the core estimation logic.
func BenchmarkStrategy_Calculate(b *testing.B) {
	strategy := DefaultStrategy()
	ctx := context.Background()

	// Setup mock data
	block := &BlockData{
		Number:       1000,
		BaseFee:      uint256.NewInt(1000000000),
		GasLimit:     30000000,
		GasUsed:      15000000,
		PriorityFees: make([]*uint256.Int, 100),
	}
	for i := range block.PriorityFees {
		block.PriorityFees[i] = uint256.NewInt(uint64(i * 1e9))
	}

	txs := make([]*TxData, 500)
	for i := range txs {
		txs[i] = &TxData{
			MaxPriorityFeePerGas: uint256.NewInt(uint64(i * 1e9)),
			MaxFeePerGas:         uint256.NewInt(uint64(i * 2e9)),
			IsEIP1559:            true,
		}
	}

	input := &CalculatorInput{
		ChainID:      1,
		CurrentBlock: block,
		RecentBlocks: []*BlockData{block, block, block, block, block},
		PendingTxs:   txs,
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = strategy.Calculate(ctx, input)
	}
}
