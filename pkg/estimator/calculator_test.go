package estimator

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/holiman/uint256"
)

func TestHybridStrategy_Calculate(t *testing.T) {
	u256 := func(v uint64) *uint256.Int {
		return uint256.NewInt(v)
	}

	makeBlock := func(number uint64, baseFee uint64, gasUsed, gasLimit uint64, priorityFees []uint64) *BlockData {
		fees := make([]*uint256.Int, len(priorityFees))
		for i, f := range priorityFees {
			fees[i] = u256(f)
		}
		return &BlockData{
			Number:       number,
			Timestamp:    time.Now(),
			BaseFee:      u256(baseFee),
			GasUsed:      gasUsed,
			GasLimit:     gasLimit,
			PriorityFees: fees,
		}
	}

	defaultStrategy := DefaultStrategy()

	tests := []struct {
		name        string
		strategy    *HybridStrategy
		input       *CalculatorInput
		wantBaseFee *uint256.Int
		wantErr     bool
	}{
		{
			name:     "Not ready (no current block)",
			strategy: defaultStrategy,
			input:    &CalculatorInput{},
			wantErr:  true,
		},
		{
			name:     "Base fee prediction - target usage",
			strategy: defaultStrategy,
			input: &CalculatorInput{
				ChainID:      1,
				CurrentBlock: makeBlock(100, 1000000000, 15000000, 30000000, nil), // 50% usage
			},
			wantBaseFee: u256(1000000000), // Should stay same
		},
		{
			name:     "Base fee prediction - full block",
			strategy: defaultStrategy,
			input: &CalculatorInput{
				ChainID:      1,
				CurrentBlock: makeBlock(100, 1000000000, 30000000, 30000000, nil), // 100% usage
			},
			// Delta = 1000000000 * (30000000 - 15000000) / 15000000 / 8 = 125000000
			// New BaseFee = 1000000000 + 125000000 = 1125000000
			wantBaseFee: u256(1125000000),
		},
		{
			name:     "Base fee prediction - empty block",
			strategy: defaultStrategy,
			input: &CalculatorInput{
				ChainID:      1,
				CurrentBlock: makeBlock(100, 1000000000, 0, 30000000, nil), // 0% usage
			},
			// Delta = 1000000000 * (15000000 - 0) / 15000000 / 8 = 125000000
			// New BaseFee = 1000000000 - 125000000 = 875000000
			wantBaseFee: u256(875000000),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.strategy.Calculate(context.Background(), tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("Calculate() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				return
			}

			if !got.BaseFee.Eq(tt.wantBaseFee) {
				t.Errorf("Calculate() BaseFee = %v, want %v", got.BaseFee, tt.wantBaseFee)
			}
		})
	}
}

func TestComputeEstimate_NoData_Uses1Wei(t *testing.T) {
	s := DefaultStrategy()
	u256 := func(v uint64) *uint256.Int { return uint256.NewInt(v) }

	baseFee := u256(1e9) // 1 gwei
	tier := DefaultTiers["standard"]

	// No historical, no mempool, no fallback -- should use 1 wei (near-zero)
	est := s.computeEstimate(baseFee, nil, nil, tier, nil)

	oneWei := uint256.NewInt(1)
	if !est.MaxPriorityFeePerGas.Eq(oneWei) {
		t.Errorf("expected 1 wei, got %v", est.MaxPriorityFeePerGas)
	}
}

func TestComputeEstimate_EmptyData_UsesFallback(t *testing.T) {
	s := DefaultStrategy()
	u256 := func(v uint64) *uint256.Int { return uint256.NewInt(v) }

	baseFee := u256(1e9) // 1 gwei
	fallback := u256(2e9) // 2 gwei from eth_maxPriorityFeePerGas
	tier := DefaultTiers["fast"]

	// No historical or mempool data -- should use fallback
	est := s.computeEstimate(baseFee, nil, nil, tier, fallback)

	if !est.MaxPriorityFeePerGas.Eq(fallback) {
		t.Errorf("expected fallback fee %v, got %v", fallback, est.MaxPriorityFeePerGas)
	}

	// MaxFeePerGas = baseFee * 1.125^4 + priorityFee
	// 1.125^4 = 1.601806640625
	// baseFee * factor = 1e9 * 1.601806 = 1601806000 (with 6-digit precision: 1601806000)
	// maxFee = 1601806000 + 2e9 = 3601806000
	wantMax := u256(3601806000)
	if !est.MaxFeePerGas.Eq(wantMax) {
		t.Errorf("expected MaxFeePerGas %v, got %v", wantMax, est.MaxFeePerGas)
	}
}

func TestComputeEstimate_FallbackClamped(t *testing.T) {
	s := DefaultStrategy()
	u256 := func(v uint64) *uint256.Int { return uint256.NewInt(v) }

	baseFee := u256(1e9)
	// Fallback exceeds safety ceiling -- should be clamped to MaxPriorityFee (500 gwei)
	fallback := u256(600e9)
	tier := DefaultTiers["urgent"]

	est := s.computeEstimate(baseFee, nil, nil, tier, fallback)

	if !est.MaxPriorityFeePerGas.Eq(s.MaxPriorityFee) {
		t.Errorf("expected clamped fee %v, got %v", s.MaxPriorityFee, est.MaxPriorityFeePerGas)
	}
}

func TestComputeEstimate_HistoricalOverFallback(t *testing.T) {
	s := DefaultStrategy()
	u256 := func(v uint64) *uint256.Int { return uint256.NewInt(v) }

	baseFee := u256(1e9)
	historical := []*uint256.Int{u256(3e9), u256(4e9), u256(5e9)}
	fallback := u256(2e9)
	tier := DefaultTiers["urgent"] // p90

	// With historical data present, fallback should NOT be used
	est := s.computeEstimate(baseFee, historical, nil, tier, fallback)

	// 90th percentile of [3e9, 4e9, 5e9] = index 1.8 -> index 1 = 4e9
	wantPriority := u256(4e9)
	if !est.MaxPriorityFeePerGas.Eq(wantPriority) {
		t.Errorf("expected historical fee %v, got %v", wantPriority, est.MaxPriorityFeePerGas)
	}
}

func TestTierDifferentiation_LowActivity(t *testing.T) {
	s := DefaultStrategy()
	u256 := func(v uint64) *uint256.Int { return uint256.NewInt(v) }

	baseFee := u256(170_000_000) // 0.17 gwei (typical low-activity chain)

	// With no fee data, all tiers get 1 wei priority fee.
	// maxFeePerGas should still differ because of per-tier base fee factors.
	tiers := s.tiers()
	slow := s.computeEstimate(baseFee, nil, nil, tiers["slow"], nil)
	standard := s.computeEstimate(baseFee, nil, nil, tiers["standard"], nil)
	fast := s.computeEstimate(baseFee, nil, nil, tiers["fast"], nil)
	urgent := s.computeEstimate(baseFee, nil, nil, tiers["urgent"], nil)

	oneWei := uint256.NewInt(1)
	for name, est := range map[string]PriorityEstimate{
		"slow": slow, "standard": standard, "fast": fast, "urgent": urgent,
	} {
		if !est.MaxPriorityFeePerGas.Eq(oneWei) {
			t.Errorf("%s: expected 1 wei, got %v", name, est.MaxPriorityFeePerGas)
		}
	}

	// maxFeePerGas should be strictly increasing: slow < standard < fast < urgent
	if !slow.MaxFeePerGas.Lt(standard.MaxFeePerGas) {
		t.Errorf("slow maxFee (%v) should be < standard (%v)", slow.MaxFeePerGas, standard.MaxFeePerGas)
	}
	if !standard.MaxFeePerGas.Lt(fast.MaxFeePerGas) {
		t.Errorf("standard maxFee (%v) should be < fast (%v)", standard.MaxFeePerGas, fast.MaxFeePerGas)
	}
	if !fast.MaxFeePerGas.Lt(urgent.MaxFeePerGas) {
		t.Errorf("fast maxFee (%v) should be < urgent (%v)", fast.MaxFeePerGas, urgent.MaxFeePerGas)
	}
}

func TestTierDifferentiation_LowFeeChain(t *testing.T) {
	s := DefaultStrategy()
	u256 := func(v uint64) *uint256.Int { return uint256.NewInt(v) }

	baseFee := u256(115_000_000) // ~0.115 gwei (typical Berachain)

	// Sub-gwei priority fees (10M to 900M wei = 0.01 to 0.9 gwei)
	historical := make([]*uint256.Int, 100)
	for i := range historical {
		fee := uint64(10_000_000) + uint64(i)*uint64(9_000_000)
		historical[i] = u256(fee)
	}

	tiers := s.tiers()
	slow := s.computeEstimate(baseFee, historical, nil, tiers["slow"], nil)
	standard := s.computeEstimate(baseFee, historical, nil, tiers["standard"], nil)
	fast := s.computeEstimate(baseFee, historical, nil, tiers["fast"], nil)
	urgent := s.computeEstimate(baseFee, historical, nil, tiers["urgent"], nil)

	// Priority fees must be differentiated, not clamped to a common floor
	if !slow.MaxPriorityFeePerGas.Lt(standard.MaxPriorityFeePerGas) {
		t.Errorf("slow priority (%v) should be < standard (%v)",
			slow.MaxPriorityFeePerGas, standard.MaxPriorityFeePerGas)
	}
	if !standard.MaxPriorityFeePerGas.Lt(fast.MaxPriorityFeePerGas) {
		t.Errorf("standard priority (%v) should be < fast (%v)",
			standard.MaxPriorityFeePerGas, fast.MaxPriorityFeePerGas)
	}
	if !fast.MaxPriorityFeePerGas.Lt(urgent.MaxPriorityFeePerGas) {
		t.Errorf("fast priority (%v) should be < urgent (%v)",
			fast.MaxPriorityFeePerGas, urgent.MaxPriorityFeePerGas)
	}

	// All priority fees should be below 1 gwei (no artificial floor)
	oneGwei := u256(1e9)
	for name, est := range map[string]PriorityEstimate{
		"slow": slow, "standard": standard, "fast": fast, "urgent": urgent,
	} {
		if !est.MaxPriorityFeePerGas.Lt(oneGwei) {
			t.Errorf("%s: priority fee %v should be < 1 gwei", name, est.MaxPriorityFeePerGas)
		}
	}
}

func TestTierDifferentiation_HighActivity(t *testing.T) {
	s := DefaultStrategy()
	u256 := func(v uint64) *uint256.Int { return uint256.NewInt(v) }

	baseFee := u256(50e9) // 50 gwei

	// Simulate a spread of priority fees with real variance
	historical := make([]*uint256.Int, 100)
	for i := range historical {
		// Fees from 0.5 to 50 gwei
		fee := uint64(500_000_000) + uint64(i)*uint64(500_000_000)
		historical[i] = u256(fee)
	}

	tiers := s.tiers()
	slow := s.computeEstimate(baseFee, historical, nil, tiers["slow"], nil)
	standard := s.computeEstimate(baseFee, historical, nil, tiers["standard"], nil)
	fast := s.computeEstimate(baseFee, historical, nil, tiers["fast"], nil)
	urgent := s.computeEstimate(baseFee, historical, nil, tiers["urgent"], nil)

	// Priority fees should be strictly increasing across tiers
	if !slow.MaxPriorityFeePerGas.Lt(standard.MaxPriorityFeePerGas) {
		t.Errorf("slow priority (%v) should be < standard (%v)",
			slow.MaxPriorityFeePerGas, standard.MaxPriorityFeePerGas)
	}
	if !standard.MaxPriorityFeePerGas.Lt(fast.MaxPriorityFeePerGas) {
		t.Errorf("standard priority (%v) should be < fast (%v)",
			standard.MaxPriorityFeePerGas, fast.MaxPriorityFeePerGas)
	}
	if !fast.MaxPriorityFeePerGas.Lt(urgent.MaxPriorityFeePerGas) {
		t.Errorf("fast priority (%v) should be < urgent (%v)",
			fast.MaxPriorityFeePerGas, urgent.MaxPriorityFeePerGas)
	}

	// MaxFeePerGas should also be strictly increasing (both factors compound)
	if !slow.MaxFeePerGas.Lt(standard.MaxFeePerGas) {
		t.Errorf("slow maxFee (%v) should be < standard (%v)", slow.MaxFeePerGas, standard.MaxFeePerGas)
	}
	if !standard.MaxFeePerGas.Lt(fast.MaxFeePerGas) {
		t.Errorf("standard maxFee (%v) should be < fast (%v)", standard.MaxFeePerGas, fast.MaxFeePerGas)
	}
	if !fast.MaxFeePerGas.Lt(urgent.MaxFeePerGas) {
		t.Errorf("fast maxFee (%v) should be < urgent (%v)", fast.MaxFeePerGas, urgent.MaxFeePerGas)
	}
}

func TestBaseFeeFactor_MatchesEIP1559(t *testing.T) {
	// Verify that baseFeeFactorForBlocks produces the correct EIP-1559 values
	tests := []struct {
		blocks int
		want   float64
	}{
		{1, 1.125},
		{2, 1.265625},   // 1.125^2
		{4, 1.601806640625}, // 1.125^4
		{6, 2.02728652954102}, // 1.125^6
	}

	for _, tt := range tests {
		got := baseFeeFactorForBlocks(tt.blocks)
		if math.Abs(got-tt.want) > 1e-10 {
			t.Errorf("baseFeeFactorForBlocks(%d) = %v, want %v", tt.blocks, got, tt.want)
		}
	}
}

func TestMulByFactor(t *testing.T) {
	u256 := func(v uint64) *uint256.Int { return uint256.NewInt(v) }

	tests := []struct {
		name   string
		val    *uint256.Int
		factor float64
		want   *uint256.Int
	}{
		{
			name:   "2x multiplier",
			val:    u256(1e9),
			factor: 2.0,
			want:   u256(2e9),
		},
		{
			name:   "1.125x (1 block protection)",
			val:    u256(1e9),
			factor: 1.125,
			want:   u256(1_125_000_000),
		},
		{
			name:   "1.125^6 (6 block protection)",
			val:    u256(1e9),
			factor: baseFeeFactorForBlocks(6),
			// 1e9 * 2.027286 (truncated to 6 decimal places) = 2027286000
			want: u256(2_027_286_000),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mulByFactor(tt.val, tt.factor)
			if !got.Eq(tt.want) {
				t.Errorf("mulByFactor(%v, %v) = %v, want %v", tt.val, tt.factor, got, tt.want)
			}
		})
	}
}

func TestHybridStrategy_Blend(t *testing.T) {
	s := DefaultStrategy()
	u256 := func(v uint64) *uint256.Int { return uint256.NewInt(v) }

	tests := []struct {
		name    string
		a, b    *uint256.Int
		weightA float64
		want    *uint256.Int
	}{
		{
			name:    "Equal weights",
			a:       u256(100),
			b:       u256(200),
			weightA: 0.5,
			want:    u256(150),
		},
		{
			name:    "Full weight A",
			a:       u256(100),
			b:       u256(200),
			weightA: 1.0,
			want:    u256(100),
		},
		{
			name:    "Full weight B",
			a:       u256(100),
			b:       u256(200),
			weightA: 0.0,
			want:    u256(200),
		},
		{
			name:    "75-25 split",
			a:       u256(100),
			b:       u256(200),
			weightA: 0.75,
			want:    u256(125),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := s.blend(tt.a, tt.b, tt.weightA)
			if !got.Eq(tt.want) {
				t.Errorf("blend() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDefaultTiers_Ordering(t *testing.T) {
	tiers := DefaultTiers

	// Percentiles should be strictly increasing
	if tiers["slow"].Percentile >= tiers["standard"].Percentile {
		t.Error("slow percentile should be < standard")
	}
	if tiers["standard"].Percentile >= tiers["fast"].Percentile {
		t.Error("standard percentile should be < fast")
	}
	if tiers["fast"].Percentile >= tiers["urgent"].Percentile {
		t.Error("fast percentile should be < urgent")
	}

	// Base fee factors should be strictly increasing
	if tiers["slow"].BaseFeeFactor >= tiers["standard"].BaseFeeFactor {
		t.Error("slow base fee factor should be < standard")
	}
	if tiers["standard"].BaseFeeFactor >= tiers["fast"].BaseFeeFactor {
		t.Error("standard base fee factor should be < fast")
	}
	if tiers["fast"].BaseFeeFactor >= tiers["urgent"].BaseFeeFactor {
		t.Error("fast base fee factor should be < urgent")
	}

	// Blocks protection should be strictly increasing
	if tiers["slow"].BlocksProtection >= tiers["standard"].BlocksProtection {
		t.Error("slow blocks protection should be < standard")
	}
	if tiers["standard"].BlocksProtection >= tiers["fast"].BlocksProtection {
		t.Error("standard blocks protection should be < fast")
	}
	if tiers["fast"].BlocksProtection >= tiers["urgent"].BlocksProtection {
		t.Error("fast blocks protection should be < urgent")
	}
}
