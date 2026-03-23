package estimator

import (
	"context"
	"log/slog"
	"slices"
	"time"

	"github.com/holiman/uint256"
)

// HybridStrategy implements a hybrid estimation approach combining:
// 1. Historical block data (what fees were accepted)
// 2. Mempool data (current competition)
// 3. Base fee prediction (EIP-1559 formula)
type HybridStrategy struct {
	// MinPriorityFee is the floor for priority fee estimates (in wei)
	// Default: 1 gwei
	MinPriorityFee *uint256.Int

	// MaxPriorityFee is the ceiling for priority fee estimates (in wei)
	// Default: 10 gwei
	MaxPriorityFee *uint256.Int

	// HistoricalWeight determines the blend between historical and mempool data
	// 0.0 = mempool only, 1.0 = historical only
	// Default: 0.3 (favor mempool for responsiveness)
	HistoricalWeight float64

	// SmoothingFactor for exponential moving average with previous estimate
	// 0.0 = no smoothing, 1.0 = ignore new data
	// Default: 0.1
	SmoothingFactor float64
}

// DefaultStrategy returns a HybridStrategy with sensible defaults.
func DefaultStrategy() *HybridStrategy {
	return &HybridStrategy{
		MinPriorityFee:   uint256.NewInt(1e9),   // 1 gwei
		MaxPriorityFee:   uint256.NewInt(10e9), // 10 gwei
		HistoricalWeight: 0.3,
		SmoothingFactor:  0.1,
	}
}

// Name returns the strategy name.
func (s *HybridStrategy) Name() string {
	return "hybrid"
}

// Calculate computes a gas estimate using the hybrid approach.
func (s *HybridStrategy) Calculate(ctx context.Context, input *CalculatorInput) (*GasEstimate, error) {
	if input.CurrentBlock == nil {
		return nil, ErrNotReady
	}

	// Predict next block's base fee
	predictedBaseFee := s.predictBaseFee(input.CurrentBlock)

	// Collect priority fees from historical blocks
	var historicalFees []*uint256.Int
	for _, block := range input.RecentBlocks {
		historicalFees = append(historicalFees, block.PriorityFees...)
	}
	slices.SortFunc(historicalFees, func(a, b *uint256.Int) int {
		if a.Lt(b) {
			return -1
		}
		if b.Lt(a) {
			return 1
		}
		return 0
	})

	// Collect priority fees from pending transactions
	var mempoolFees []*uint256.Int
	for _, tx := range input.PendingTxs {
		fee := tx.EffectivePriorityFee(predictedBaseFee)
		if !fee.IsZero() {
			mempoolFees = append(mempoolFees, fee)
		}
	}
	slices.SortFunc(mempoolFees, func(a, b *uint256.Int) int {
		if a.Lt(b) {
			return -1
		}
		if b.Lt(a) {
			return 1
		}
		return 0
	})

	// Compute estimates at each confidence level
	estimate := &GasEstimate{
		ChainID:     input.ChainID,
		BlockNumber: input.CurrentBlock.Number,
		Timestamp:   time.Now(),
		BaseFee:     predictedBaseFee,
		Urgent:      s.computeEstimate(predictedBaseFee, historicalFees, mempoolFees, 0.99, input.FallbackPriorityFee),
		Fast:        s.computeEstimate(predictedBaseFee, historicalFees, mempoolFees, 0.90, input.FallbackPriorityFee),
		Standard:    s.computeEstimate(predictedBaseFee, historicalFees, mempoolFees, 0.50, input.FallbackPriorityFee),
		Slow:        s.computeEstimate(predictedBaseFee, historicalFees, mempoolFees, 0.25, input.FallbackPriorityFee),
	}

	// Apply smoothing if we have a previous estimate
	if input.PreviousEstimate != nil && s.SmoothingFactor > 0 {
		estimate = s.smooth(estimate, input.PreviousEstimate)
	}

	return estimate, nil
}

// predictBaseFee predicts the base fee for the next block using EIP-1559 formula.
func (s *HybridStrategy) predictBaseFee(block *BlockData) *uint256.Int {
	if block.BaseFee == nil {
		return uint256.NewInt(1e9) // 1 gwei default for non-EIP-1559
	}

	baseFee := new(uint256.Int).Set(block.BaseFee)
	gasTarget := block.GasLimit / 2

	if block.GasUsed == gasTarget {
		return baseFee
	}

	if block.GasUsed > gasTarget {
		// Block was more than 50% full - base fee increases
		delta := new(uint256.Int).Mul(baseFee, uint256.NewInt(block.GasUsed-gasTarget))
		delta.Div(delta, uint256.NewInt(gasTarget))
		delta.Div(delta, uint256.NewInt(8)) // max 12.5% change
		baseFee.Add(baseFee, delta)
	} else {
		// Block was less than 50% full - base fee decreases
		delta := new(uint256.Int).Mul(baseFee, uint256.NewInt(gasTarget-block.GasUsed))
		delta.Div(delta, uint256.NewInt(gasTarget))
		delta.Div(delta, uint256.NewInt(8))
		// Check for underflow
		if baseFee.Lt(delta) {
			baseFee.SetUint64(0)
		} else {
			baseFee.Sub(baseFee, delta)
		}
	}

	return baseFee
}

// computeEstimate calculates priority fee at a given percentile.
// fallbackFee is the node's suggested priority fee from eth_maxPriorityFeePerGas (may be nil).
func (s *HybridStrategy) computeEstimate(
	baseFee *uint256.Int,
	historical []*uint256.Int,
	mempool []*uint256.Int,
	percentile float64,
	fallbackFee *uint256.Int,
) PriorityEstimate {
	var priorityFee *uint256.Int

	histP := s.percentile(historical, percentile)
	mempP := s.percentile(mempool, percentile)

	if histP != nil && mempP != nil {
		// Blend historical and mempool estimates
		weighted := s.blend(histP, mempP, s.HistoricalWeight)
		priorityFee = weighted
	} else if mempP != nil {
		priorityFee = mempP
	} else if histP != nil {
		priorityFee = histP
	} else if fallbackFee != nil {
		// Use node's eth_maxPriorityFeePerGas as fallback
		slog.Warn("no historical or mempool fee data, using eth_maxPriorityFeePerGas fallback",
			"fallback_wei", fallbackFee.String(),
			"percentile", percentile,
		)
		priorityFee = new(uint256.Int).Set(fallbackFee)
	} else {
		// Last resort: interpolate between min and max based on percentile
		slog.Warn("no fee data available, using hardcoded default priority fee",
			"percentile", percentile,
			"min_wei", s.MinPriorityFee.String(),
			"max_wei", s.MaxPriorityFee.String(),
		)
		priorityFee = s.defaultPriorityFee(percentile)
	}

	// Clamp to min/max
	priorityFee = s.clamp(priorityFee)

	// Calculate maxFeePerGas: baseFee * 2 + priorityFee
	// The 2x buffer handles up to ~6 consecutive full blocks
	maxFee := new(uint256.Int).Mul(baseFee, uint256.NewInt(2))
	maxFee.Add(maxFee, priorityFee)

	return PriorityEstimate{
		MaxPriorityFeePerGas: priorityFee,
		MaxFeePerGas:         maxFee,
		Confidence:           percentile,
	}
}

// percentile calculates the value at the given percentile (0.0 to 1.0).
// Assumes values is already sorted.
func (s *HybridStrategy) percentile(values []*uint256.Int, p float64) *uint256.Int {
	if len(values) == 0 {
		return nil
	}

	// Calculate index
	idx := int(float64(len(values)-1) * p)
	return new(uint256.Int).Set(values[idx])
}

// blend computes a weighted average of two uint256.Int values.
func (s *HybridStrategy) blend(a, b *uint256.Int, weightA float64) *uint256.Int {
	// result = a * weightA + b * (1 - weightA)
	// Using integer math: result = (a * wA + b * wB) / 100 where wA + wB = 100
	wA := uint64(weightA * 100)
	wB := 100 - wA

	aWeighted := new(uint256.Int).Mul(a, uint256.NewInt(wA))
	bWeighted := new(uint256.Int).Mul(b, uint256.NewInt(wB))

	result := new(uint256.Int).Add(aWeighted, bWeighted)
	result.Div(result, uint256.NewInt(100))

	return result
}

// defaultPriorityFee returns a sensible default based on confidence level.
func (s *HybridStrategy) defaultPriorityFee(percentile float64) *uint256.Int {
	// Scale between min and max based on percentile
	// Higher percentile = higher fee
	min := new(uint256.Int).Set(s.MinPriorityFee)
	max := new(uint256.Int).Set(s.MaxPriorityFee)

	diff := new(uint256.Int).Sub(max, min)
	scaled := new(uint256.Int).Mul(diff, uint256.NewInt(uint64(percentile*100)))
	scaled.Div(scaled, uint256.NewInt(100))

	return new(uint256.Int).Add(min, scaled)
}

// clamp ensures the priority fee is within bounds.
func (s *HybridStrategy) clamp(fee *uint256.Int) *uint256.Int {
	if fee.Lt(s.MinPriorityFee) {
		return new(uint256.Int).Set(s.MinPriorityFee)
	}
	if fee.Gt(s.MaxPriorityFee) {
		return new(uint256.Int).Set(s.MaxPriorityFee)
	}
	return fee
}

// smooth applies exponential smoothing with the previous estimate.
func (s *HybridStrategy) smooth(current, previous *GasEstimate) *GasEstimate {
	factor := s.SmoothingFactor

	return &GasEstimate{
		ChainID:     current.ChainID,
		BlockNumber: current.BlockNumber,
		Timestamp:   current.Timestamp,
		BaseFee:     current.BaseFee, // Don't smooth base fee
		Urgent:      s.smoothEstimate(current.Urgent, previous.Urgent, factor),
		Fast:        s.smoothEstimate(current.Fast, previous.Fast, factor),
		Standard:    s.smoothEstimate(current.Standard, previous.Standard, factor),
		Slow:        s.smoothEstimate(current.Slow, previous.Slow, factor),
	}
}

func (s *HybridStrategy) smoothEstimate(current, previous PriorityEstimate, factor float64) PriorityEstimate {
	// new = current * (1 - factor) + previous * factor
	smoothedPriority := s.blend(previous.MaxPriorityFeePerGas, current.MaxPriorityFeePerGas, factor)
	smoothedMax := s.blend(previous.MaxFeePerGas, current.MaxFeePerGas, factor)

	return PriorityEstimate{
		MaxPriorityFeePerGas: smoothedPriority,
		MaxFeePerGas:         smoothedMax,
		Confidence:           current.Confidence,
	}
}

// Verify interface compliance at compile time.
var _ Strategy = (*HybridStrategy)(nil)
