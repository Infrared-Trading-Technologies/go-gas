package estimator

import (
	"context"
	"testing"
	"time"

	"github.com/Infrared-Trading-Technologies/go-gas/pkg/eth"
	"github.com/holiman/uint256"
)

func TestEstimator_Run(t *testing.T) {
	// Setup mocks
	mockClient := &mockBlockReader{
		chainIDFunc: func(ctx context.Context) (uint64, error) {
			return 1, nil
		},
		latestBlockFunc: func(ctx context.Context) (*eth.Block, error) {
			return &eth.Block{
				Number:  100,
				BaseFee: uint256.NewInt(1000000000),
			}, nil
		},
		blockByNumberFunc: func(ctx context.Context, number *uint256.Int) (*eth.Block, error) {
			return &eth.Block{
				Number:  number.Uint64(),
				BaseFee: uint256.NewInt(1000000000),
			}, nil
		},
	}

	mockTx := &mockTxReader{}

	mockSub := &mockSubscriber{
		subHeadsFunc: func(ctx context.Context) (<-chan *eth.Block, error) {
			ch := make(chan *eth.Block)
			return ch, nil
		},
		subPendingFunc: func(ctx context.Context) (<-chan string, error) {
			ch := make(chan string)
			return ch, nil
		},
	}

	provider := NewProvider()

	e := New(mockClient, mockTx, mockSub, provider, WithHistorySize(5))

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := e.Run(ctx)
	if err != nil {
		t.Errorf("Run() error = %v", err)
	}
}
