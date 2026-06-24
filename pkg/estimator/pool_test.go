package estimator

import (
	"testing"

	"github.com/Infrared-Trading-Technologies/go-gas/pkg/eth"
	"github.com/holiman/uint256"
)

func TestLocalTxPool(t *testing.T) {
	pool := NewLocalTxPool(3)

	// Helper to create tx
	makeTx := func(fee uint64) *eth.Transaction {
		return &eth.Transaction{
			Type:                 2, // EIP-1559
			MaxPriorityFeePerGas: uint256.NewInt(fee),
			MaxFeePerGas:         uint256.NewInt(fee * 2),
		}
	}

	// Add 1, 2, 3
	pool.Add(makeTx(10))
	pool.Add(makeTx(20))
	pool.Add(makeTx(30))

	snap := pool.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("Snapshot len = %d, want 3", len(snap))
	}

	// Check order (oldest first)
	if snap[0].MaxPriorityFeePerGas.Uint64() != 10 {
		t.Errorf("snap[0] fee = %d, want 10", snap[0].MaxPriorityFeePerGas.Uint64())
	}
	if snap[2].MaxPriorityFeePerGas.Uint64() != 30 {
		t.Errorf("snap[2] fee = %d, want 30", snap[2].MaxPriorityFeePerGas.Uint64())
	}

	// Add 4 (should overwrite 10)
	pool.Add(makeTx(40))

	snap = pool.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("Snapshot len = %d, want 3", len(snap))
	}
	if snap[0].MaxPriorityFeePerGas.Uint64() != 20 {
		t.Errorf("snap[0] fee = %d, want 20", snap[0].MaxPriorityFeePerGas.Uint64())
	}
	if snap[2].MaxPriorityFeePerGas.Uint64() != 40 {
		t.Errorf("snap[2] fee = %d, want 40", snap[2].MaxPriorityFeePerGas.Uint64())
	}
}
