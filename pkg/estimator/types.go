package estimator

import (
	"time"

	"github.com/holiman/uint256"
)

// GasEstimate represents a point-in-time gas price estimate.
// This struct is immutable - all fields are either value types or
// treated as read-only. Safe to share across goroutines.
type GasEstimate struct {
	// Chain and block context
	ChainID     uint64
	BlockNumber uint64
	Timestamp   time.Time

	// Predicted base fee for next block (EIP-1559)
	BaseFee *uint256.Int

	// Priority fee estimates at different confidence levels.
	// Each tier uses a different percentile of the fee distribution and a different
	// base fee buffer (1.125^N) derived from EIP-1559 math.
	Urgent   PriorityEstimate // p90, 6-block base fee protection
	Fast     PriorityEstimate // p50, 4-block base fee protection
	Standard PriorityEstimate // p25, 2-block base fee protection
	Slow     PriorityEstimate // p10, 1-block base fee protection
}

// PriorityEstimate represents a gas estimate at a specific confidence level.
type PriorityEstimate struct {
	// MaxPriorityFeePerGas is the tip to miners/validators
	MaxPriorityFeePerGas *uint256.Int

	// MaxFeePerGas is the total max fee (baseFee * factor + priorityFee)
	// where factor = 1.125^N, derived from the tier's base fee protection level
	MaxFeePerGas *uint256.Int

	// Confidence is the probability of inclusion (0.0 to 1.0)
	Confidence float64
}

// CalculatorInput contains all data needed to compute a gas estimate.
// Used to decouple the calculation logic from data fetching.
type CalculatorInput struct {
	ChainID          uint64
	CurrentBlock     *BlockData
	RecentBlocks     []*BlockData
	PendingTxs       []*TxData
	PreviousEstimate *GasEstimate

	// FallbackPriorityFee is the node's suggested priority fee from eth_maxPriorityFeePerGas.
	// Used when both historical and mempool data are empty. Nil if unavailable.
	FallbackPriorityFee *uint256.Int
}

// BlockData is a simplified view of block data for calculations.
type BlockData struct {
	Number       uint64
	Timestamp    time.Time
	BaseFee      *uint256.Int
	GasUsed      uint64
	GasLimit     uint64
	PriorityFees []*uint256.Int // priority fees from included transactions
}

// GasUtilization returns the ratio of gas used to gas limit.
func (b *BlockData) GasUtilization() float64 {
	if b.GasLimit == 0 {
		return 0
	}
	return float64(b.GasUsed) / float64(b.GasLimit)
}

// TxData is a simplified view of pending transaction data.
type TxData struct {
	MaxPriorityFeePerGas *uint256.Int
	MaxFeePerGas         *uint256.Int
	GasPrice             *uint256.Int // for legacy transactions
	IsEIP1559            bool
}

// EffectivePriorityFee returns the priority fee that would be paid given a base fee.
func (t *TxData) EffectivePriorityFee(baseFee *uint256.Int) *uint256.Int {
	if baseFee == nil || baseFee.IsZero() {
		if t.IsEIP1559 && t.MaxPriorityFeePerGas != nil {
			return new(uint256.Int).Set(t.MaxPriorityFeePerGas)
		}
		if t.GasPrice != nil {
			return new(uint256.Int).Set(t.GasPrice)
		}
		return uint256.NewInt(0)
	}

	if t.IsEIP1559 && t.MaxFeePerGas != nil && t.MaxPriorityFeePerGas != nil {
		if t.MaxFeePerGas.Lt(baseFee) {
			return uint256.NewInt(0)
		}
		maxMinusBase := new(uint256.Int).Sub(t.MaxFeePerGas, baseFee)

		if t.MaxPriorityFeePerGas.Lt(maxMinusBase) {
			return new(uint256.Int).Set(t.MaxPriorityFeePerGas)
		}
		return maxMinusBase
	}

	if t.GasPrice != nil {
		if t.GasPrice.Lt(baseFee) {
			return uint256.NewInt(0)
		}
		return new(uint256.Int).Sub(t.GasPrice, baseFee)
	}

	return uint256.NewInt(0)
}
