// Package main is the entry point for the gas estimator service.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Infrared-Trading-Technologies/go-gas/internal/api/grpc"
	"github.com/Infrared-Trading-Technologies/go-gas/internal/config"
	"github.com/Infrared-Trading-Technologies/go-gas/internal/observability"
	"github.com/Infrared-Trading-Technologies/go-gas/pkg/estimator"
	"github.com/Infrared-Trading-Technologies/go-gas/pkg/eth"
	"github.com/Infrared-Trading-Technologies/go-gas/pkg/health"
)

func main() {
	// Root context canceled on SIGTERM/SIGINT (12-factor: disposability)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	code := 0
	if err := run(ctx); err != nil {
		slog.Error("fatal error", "error", err)
		code = 1
	}

	os.Exit(code)
}

func run(ctx context.Context) error {
	// Load configuration from environment (12-factor: config)
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	// Initialize structured logging (12-factor: logs as streams)
	logger := observability.NewLogger(cfg.LogLevel, cfg.LogFormat)
	slog.SetDefault(logger)

	slog.Info("starting gas estimator",
		"grpc_addr", cfg.GRPCAddr,
		"http_addr", cfg.HTTPAddr,
		"history_blocks", cfg.HistoryBlocks,
		"mempool_samples", cfg.MempoolSamples,
		"recalc_interval", cfg.RecalcInterval,
	)

	// Build dependency graph (dependency inversion)

	// 1. Eth client (HTTP for RPC calls)
	ethClient := eth.NewClient(cfg.NodeHTTPURL)
	defer ethClient.Close()

	// 2. WebSocket subscriber for real-time updates
	subscriber := eth.NewWSSubscriber(cfg.NodeWSURL, logger)
	defer subscriber.Close()

	// 3. Provider (atomic estimate storage)
	provider := estimator.NewProvider()

	// 4. Strategy (estimation algorithm)
	strategy := estimator.DefaultStrategy()

	// 5. Estimator (orchestrates everything)
	est := estimator.New(
		ethClient,
		ethClient, // also implements TransactionReader
		subscriber,
		provider,
		estimator.WithHistorySize(cfg.HistoryBlocks),
		estimator.WithMempoolSamples(cfg.MempoolSamples),
		estimator.WithRecalcInterval(cfg.RecalcInterval),
		estimator.WithStrategy(strategy),
		estimator.WithLogger(logger),
	)

	// 6. API server
	apiServer := grpc.NewServer(cfg.GRPCAddr, provider, logger)

	// 7. Health server
	healthServer := health.NewServer(cfg.HTTPAddr, provider, logger)

	// Run all components concurrently
	errCh := make(chan error, 3)

	go func() {
		if err := est.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			errCh <- fmt.Errorf("estimator: %w", err)
		}
	}()

	go func() {
		if err := apiServer.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			errCh <- fmt.Errorf("api server: %w", err)
		}
	}()

	go func() {
		if err := healthServer.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			errCh <- fmt.Errorf("health server: %w", err)
		}
	}()

	// Wait for shutdown signal or error
	select {
	case <-ctx.Done():
		slog.Info("received shutdown signal")
	case err := <-errCh:
		slog.Error("component failed", "error", err)
		return err
	}

	// Graceful shutdown with timeout
	slog.Info("shutting down gracefully")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Shutdown in reverse dependency order
	if err := apiServer.Shutdown(shutdownCtx); err != nil {
		slog.Warn("api server shutdown error", "error", err)
	}

	if err := healthServer.Shutdown(shutdownCtx); err != nil {
		slog.Warn("health server shutdown error", "error", err)
	}

	slog.Info("shutdown complete")
	return nil
}
