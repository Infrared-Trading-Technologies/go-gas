package estimator

import (
	"context"
	"testing"
	"time"

	"github.com/holiman/uint256"
)

func TestHybridStrategy_Calculate(t *testing.T) {
	// Helper to create uint256.Int
	u256 := func(v uint64) *uint256.Int {
		return uint256.NewInt(v)
	}

	// Helper to create a block
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
		wantUrgent  *uint256.Int // Expected Urgent Priority Fee
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
			// Delta = 1000000000 * (30000000 - 15000000) / 15000000 / 8 = 1000000000 * 1 / 8 = 125000000
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
		{
			name:     "No data - defaults",
			strategy: defaultStrategy,
			input: &CalculatorInput{
				ChainID:      1,
				CurrentBlock: makeBlock(100, 1000000000, 15000000, 30000000, nil),
			},
			wantBaseFee: u256(1000000000),
			// Default Urgent (99%)
			// Min: 1 gwei, Max: 10 gwei
			// Diff: 9 gwei
			// Scaled: 9 * 99 / 100 = 8.91 gwei
			// Result: 1 + 8.91 = 9.91 gwei = 9910000000
			wantUrgent: u256(9910000000),
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

			if tt.wantUrgent != nil {
				if !got.Urgent.MaxPriorityFeePerGas.Eq(tt.wantUrgent) {
					t.Errorf("Calculate() Urgent Priority = %v, want %v", got.Urgent.MaxPriorityFeePerGas, tt.wantUrgent)
				}
			}
		})
	}
}

func TestComputeEstimate_EmptyData_UsesFallback(t *testing.T) {
	s := DefaultStrategy()
	u256 := func(v uint64) *uint256.Int { return uint256.NewInt(v) }

	baseFee := u256(1e9) // 1 gwei
	fallback := u256(2e9) // 2 gwei from eth_maxPriorityFeePerGas

	// No historical or mempool data -- should use fallback
	est := s.computeEstimate(baseFee, nil, nil, 0.90, fallback)

	if !est.MaxPriorityFeePerGas.Eq(fallback) {
		t.Errorf("expected fallback fee %v, got %v", fallback, est.MaxPriorityFeePerGas)
	}

	// MaxFeePerGas = baseFee*2 + priorityFee = 2e9 + 2e9 = 4e9
	wantMax := u256(4e9)
	if !est.MaxFeePerGas.Eq(wantMax) {
		t.Errorf("expected MaxFeePerGas %v, got %v", wantMax, est.MaxFeePerGas)
	}
}

func TestComputeEstimate_EmptyData_NoFallback_UsesDefault(t *testing.T) {
	s := DefaultStrategy()
	u256 := func(v uint64) *uint256.Int { return uint256.NewInt(v) }

	baseFee := u256(1e9)

	// No historical, no mempool, no fallback -- should use defaultPriorityFee
	est := s.computeEstimate(baseFee, nil, nil, 0.90, nil)

	// With 10 gwei ceiling: diff=9 gwei, scaled=9*90/100=8.1 gwei, result=1+8.1=9.1 gwei
	wantPriority := u256(9100000000)
	if !est.MaxPriorityFeePerGas.Eq(wantPriority) {
		t.Errorf("expected default priority fee %v, got %v", wantPriority, est.MaxPriorityFeePerGas)
	}

	// Verify it's reasonable (under 10 gwei ceiling)
	ceiling := u256(10e9)
	if est.MaxPriorityFeePerGas.Gt(ceiling) {
		t.Errorf("priority fee %v exceeds ceiling %v", est.MaxPriorityFeePerGas, ceiling)
	}
}

func TestComputeEstimate_FallbackClamped(t *testing.T) {
	s := DefaultStrategy()
	u256 := func(v uint64) *uint256.Int { return uint256.NewInt(v) }

	baseFee := u256(1e9)
	// Fallback exceeds ceiling -- should be clamped to MaxPriorityFee (10 gwei)
	fallback := u256(500e9)

	est := s.computeEstimate(baseFee, nil, nil, 0.90, fallback)

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

	// With historical data present, fallback should NOT be used
	est := s.computeEstimate(baseFee, historical, nil, 0.90, fallback)

	// 90th percentile of [3e9, 4e9, 5e9] = index 1.8 -> index 1 = 4e9
	wantPriority := u256(4e9)
	if !est.MaxPriorityFeePerGas.Eq(wantPriority) {
		t.Errorf("expected historical fee %v, got %v", wantPriority, est.MaxPriorityFeePerGas)
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
			// 100 * 0.75 + 200 * 0.25 = 75 + 50 = 125
			want: u256(125),
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
