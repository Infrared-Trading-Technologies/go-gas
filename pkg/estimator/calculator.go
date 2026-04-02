package estimator

import (
	"context"
	"log/slog"
	"math"
	"slices"
	"time"

	"github.com/holiman/uint256"
)

// TierConfig defines the parameters for a single gas estimation tier.
// All values are derived from EIP-1559 math and standard statistical breakpoints.
type TierConfig struct {
	// Percentile of the blended historical+mempool priority fee distribution (0.0 to 1.0).
	Percentile float64

	// BaseFeeFactor is the multiplier applied to the predicted base fee for maxFeePerGas.
	// Derived from EIP-1559: 1.125^N where N is the number of consecutive full blocks
	// of base fee increase the transaction should survive without becoming unincludable.
	BaseFeeFactor float64

	// BlocksProtection is the semantic meaning behind BaseFeeFactor: how many consecutive
	// full blocks of worst-case base fee increase this tier protects against.
	BlocksProtection int
}

// baseFeeFactorForBlocks computes 1.125^n (the EIP-1559 max increase per block raised to n).
func baseFeeFactorForBlocks(n int) float64 {
	return math.Pow(1.125, float64(n))
}

// HybridStrategy implements a hybrid estimation approach combining:
// 1. Historical block data (what fees were accepted)
// 2. Mempool data (current competition)
// 3. Base fee prediction (EIP-1559 formula)
// 4. Per-tier differentiation via percentiles and EIP-1559-derived base fee factors
type HybridStrategy struct {
	// MinPriorityFee is the relay floor for priority fee estimates (in wei).
	// Derived from Geth's default txpool.pricelimit. Transactions below this
	// won't be relayed by default-configured nodes.
	// Applied uniformly across all tiers (not per-tier).
	// Default: 1 gwei
	MinPriorityFee *uint256.Int

	// MaxPriorityFee is a safety ceiling for priority fee estimates (in wei).
	// Set high to avoid capping during genuine gas spikes.
	// Default: 500 gwei
	MaxPriorityFee *uint256.Int

	// HistoricalWeight determines the blend between historical and mempool data.
	// 0.0 = mempool only, 1.0 = historical only
	// Default: 0.3 (favor mempool for responsiveness)
	HistoricalWeight float64

	// SmoothingFactor for exponential moving average with previous estimate.
	// 0.0 = no smoothing, 1.0 = ignore new data
	// Default: 0.1
	SmoothingFactor float64

	// Tiers defines the configuration for each estimation tier.
	// If nil, DefaultTiers is used.
	Tiers map[string]TierConfig
}

// DefaultTiers defines tier configurations derived from EIP-1559 math and
// standard statistical percentile breakpoints.
//
// Percentiles: p10/p25/p50/p90 provide meaningful spread without p99 outlier sensitivity.
// Base fee factors: 1.125^N where N = blocks of consecutive full-block base fee increase
// the transaction can survive.
var DefaultTiers = map[string]TierConfig{
	"slow":     {Percentile: 0.10, BaseFeeFactor: baseFeeFactorForBlocks(1), BlocksProtection: 1},
	"standard": {Percentile: 0.25, BaseFeeFactor: baseFeeFactorForBlocks(2), BlocksProtection: 2},
	"fast":     {Percentile: 0.50, BaseFeeFactor: baseFeeFactorForBlocks(4), BlocksProtection: 4},
	"urgent":   {Percentile: 0.90, BaseFeeFactor: baseFeeFactorForBlocks(6), BlocksProtection: 6},
}

// DefaultStrategy returns a HybridStrategy with sensible defaults.
func DefaultStrategy() *HybridStrategy {
	return &HybridStrategy{
		MinPriorityFee:   uint256.NewInt(1e9),   // 1 gwei (Geth txpool.pricelimit default)
		MaxPriorityFee:   uint256.NewInt(500e9),  // 500 gwei (safety ceiling only)
		HistoricalWeight: 0.3,
		SmoothingFactor:  0.1,
		Tiers:            DefaultTiers,
	}
}

// Name returns the strategy name.
func (s *HybridStrategy) Name() string {
	return "hybrid"
}

// tiers returns the configured tier map, falling back to DefaultTiers.
func (s *HybridStrategy) tiers() map[string]TierConfig {
	if s.Tiers != nil {
		return s.Tiers
	}
	return DefaultTiers
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

	tiers := s.tiers()

	// Compute estimates at each confidence level using per-tier config
	estimate := &GasEstimate{
		ChainID:     input.ChainID,
		BlockNumber: input.CurrentBlock.Number,
		Timestamp:   time.Now(),
		BaseFee:     predictedBaseFee,
		Urgent:      s.computeEstimate(predictedBaseFee, historicalFees, mempoolFees, tiers["urgent"], input.FallbackPriorityFee),
		Fast:        s.computeEstimate(predictedBaseFee, historicalFees, mempoolFees, tiers["fast"], input.FallbackPriorityFee),
		Standard:    s.computeEstimate(predictedBaseFee, historicalFees, mempoolFees, tiers["standard"], input.FallbackPriorityFee),
		Slow:        s.computeEstimate(predictedBaseFee, historicalFees, mempoolFees, tiers["slow"], input.FallbackPriorityFee),
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
		if baseFee.Lt(delta) {
			baseFee.SetUint64(0)
		} else {
			baseFee.Sub(baseFee, delta)
		}
	}

	return baseFee
}

// computeEstimate calculates priority fee and max fee for a given tier.
// The tier's percentile determines where in the fee distribution to sample,
// and the tier's base fee factor determines the base fee buffer in maxFeePerGas.
func (s *HybridStrategy) computeEstimate(
	baseFee *uint256.Int,
	historical []*uint256.Int,
	mempool []*uint256.Int,
	tier TierConfig,
	fallbackFee *uint256.Int,
) PriorityEstimate {
	var priorityFee *uint256.Int

	histP := s.percentile(historical, tier.Percentile)
	mempP := s.percentile(mempool, tier.Percentile)

	if histP != nil && mempP != nil {
		priorityFee = s.blend(histP, mempP, s.HistoricalWeight)
	} else if mempP != nil {
		priorityFee = mempP
	} else if histP != nil {
		priorityFee = histP
	} else if fallbackFee != nil {
		slog.Warn("no historical or mempool fee data, using eth_maxPriorityFeePerGas fallback",
			"fallback_wei", fallbackFee.String(),
			"percentile", tier.Percentile,
		)
		priorityFee = new(uint256.Int).Set(fallbackFee)
	} else {
		slog.Warn("no fee data available, using relay minimum as priority fee",
			"percentile", tier.Percentile,
			"min_wei", s.MinPriorityFee.String(),
		)
		priorityFee = new(uint256.Int).Set(s.MinPriorityFee)
	}

	// Apply relay floor and safety ceiling
	priorityFee = s.clamp(priorityFee)

	// Calculate maxFeePerGas using per-tier base fee factor.
	// factor = 1.125^N where N = blocks of base fee protection for this tier.
	// maxFeePerGas = baseFee * factor + priorityFee
	maxFee := mulByFactor(baseFee, tier.BaseFeeFactor)
	maxFee.Add(maxFee, priorityFee)

	return PriorityEstimate{
		MaxPriorityFeePerGas: priorityFee,
		MaxFeePerGas:         maxFee,
		Confidence:           tier.Percentile,
	}
}

// mulByFactor multiplies a uint256.Int by a float64 factor using fixed-point arithmetic.
// Precision: 6 decimal places (multiply by 1e6, then divide).
func mulByFactor(val *uint256.Int, factor float64) *uint256.Int {
	const precision = 1_000_000
	factorInt := uint64(factor * precision)
	result := new(uint256.Int).Mul(val, uint256.NewInt(factorInt))
	result.Div(result, uint256.NewInt(precision))
	return result
}

// percentile calculates the value at the given percentile (0.0 to 1.0).
// Assumes values is already sorted.
func (s *HybridStrategy) percentile(values []*uint256.Int, p float64) *uint256.Int {
	if len(values) == 0 {
		return nil
	}

	idx := int(float64(len(values)-1) * p)
	return new(uint256.Int).Set(values[idx])
}

// blend computes a weighted average of two uint256.Int values.
func (s *HybridStrategy) blend(a, b *uint256.Int, weightA float64) *uint256.Int {
	wA := uint64(weightA * 100)
	wB := 100 - wA

	aWeighted := new(uint256.Int).Mul(a, uint256.NewInt(wA))
	bWeighted := new(uint256.Int).Mul(b, uint256.NewInt(wB))

	result := new(uint256.Int).Add(aWeighted, bWeighted)
	result.Div(result, uint256.NewInt(100))

	return result
}

// clamp ensures the priority fee is within the relay floor and safety ceiling.
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
