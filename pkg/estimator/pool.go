package estimator

import (
	"sync"

	"github.com/Infrared-Trading-Technologies/go-gas/pkg/eth"
)

// LocalTxPool maintains a ring buffer of recent pending transactions.
// It provides a low-latency view of the mempool without polling full content.
type LocalTxPool struct {
	mu    sync.RWMutex
	txs   []*TxData
	size  int
	pos   int
	count int
}

// NewLocalTxPool creates a new local transaction pool.
func NewLocalTxPool(size int) *LocalTxPool {
	return &LocalTxPool{
		txs:  make([]*TxData, size),
		size: size,
	}
}

// Add adds a transaction to the pool.
func (p *LocalTxPool) Add(tx *eth.Transaction) {
	// Only track EIP-1559 or legacy txs with gas price
	data := &TxData{
		IsEIP1559: tx.IsEIP1559(),
	}

	if tx.IsEIP1559() {
		if tx.MaxPriorityFeePerGas != nil {
			data.MaxPriorityFeePerGas = tx.MaxPriorityFeePerGas
		}
		if tx.MaxFeePerGas != nil {
			data.MaxFeePerGas = tx.MaxFeePerGas
		}
	} else {
		if tx.GasPrice != nil {
			data.GasPrice = tx.GasPrice
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.txs[p.pos] = data
	p.pos = (p.pos + 1) % p.size
	if p.count < p.size {
		p.count++
	}
}

// Snapshot returns a copy of all transactions in the pool.
func (p *LocalTxPool) Snapshot() []*TxData {
	p.mu.RLock()
	defer p.mu.RUnlock()

	res := make([]*TxData, 0, p.count)
	for i := 0; i < p.count; i++ {
		// Calculate index starting from oldest
		idx := (p.pos - p.count + i + p.size) % p.size
		if p.txs[idx] != nil {
			res = append(res, p.txs[idx])
		}
	}
	return res
}
