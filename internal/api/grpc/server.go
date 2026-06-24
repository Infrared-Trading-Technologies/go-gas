// Package grpc provides the gRPC API server for gas estimates.
package grpc

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/Infrared-Trading-Technologies/go-gas/pkg/estimator"
)

// Note: This is a simplified HTTP/JSON implementation.
// In production, replace with proper gRPC using protobuf.
// The interface is designed to be easily swapped.

// Server provides the gas estimation API.
type Server struct {
	addr     string
	provider estimator.EstimateReader
	logger   *slog.Logger
	server   *http.Server
}

// NewServer creates a new gRPC server.
func NewServer(addr string, provider estimator.EstimateReader, logger *slog.Logger) *Server {
	s := &Server{
		addr:     addr,
		provider: provider,
		logger:   logger.With("component", "grpc"),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/gas/estimate", s.handleEstimate)
	mux.HandleFunc("/v1/gas/estimate/stream", s.handleStream)

	s.server = &http.Server{
		Addr:         addr,
		Handler:      s.withMiddleware(mux),
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	return s
}

// Run starts the server. Blocks until context is canceled.
func (s *Server) Run(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("listening: %w", err)
	}

	errCh := make(chan error, 1)
	go func() {
		s.logger.Info("API server starting", "addr", s.addr)
		if err := s.server.Serve(listener); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		return nil
	case err := <-errCh:
		return err
	}
}

// Shutdown gracefully stops the server.
func (s *Server) Shutdown(ctx context.Context) error {
	s.logger.Info("API server shutting down")
	return s.server.Shutdown(ctx)
}

// withMiddleware wraps the handler with common middleware.
func (s *Server) withMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// Set common headers
		w.Header().Set("Content-Type", "application/json")

		// CORS for development
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)

		s.logger.Debug("request completed",
			"method", r.Method,
			"path", r.URL.Path,
			"duration_us", time.Since(start).Microseconds(),
		)
	})
}

// GasEstimateResponse is the API response format.
type GasEstimateResponse struct {
	ChainID     uint64          `json:"chain_id"`
	BlockNumber uint64          `json:"block_number"`
	Timestamp   string          `json:"timestamp"`
	BaseFee     string          `json:"base_fee"`
	Estimates   EstimatesBundle `json:"estimates"`
}

// EstimatesBundle contains all priority level estimates.
type EstimatesBundle struct {
	Urgent   EstimateLevel `json:"urgent"`
	Fast     EstimateLevel `json:"fast"`
	Standard EstimateLevel `json:"standard"`
	Slow     EstimateLevel `json:"slow"`
}

// EstimateLevel represents a single priority level estimate.
type EstimateLevel struct {
	MaxPriorityFeePerGas string  `json:"max_priority_fee_per_gas"`
	MaxFeePerGas         string  `json:"max_fee_per_gas"`
	Confidence           float64 `json:"confidence"`
}

// handleEstimate returns the current gas estimate.
func (s *Server) handleEstimate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 100*time.Millisecond)
	defer cancel()

	est, err := s.provider.Current(ctx)
	if err != nil {
		if err == estimator.ErrNotReady {
			s.writeError(w, http.StatusServiceUnavailable, "estimator not ready")
			return
		}
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	resp := GasEstimateResponse{
		ChainID:     est.ChainID,
		BlockNumber: est.BlockNumber,
		Timestamp:   est.Timestamp.UTC().Format(time.RFC3339Nano),
		BaseFee:     est.BaseFee.String(),
		Estimates: EstimatesBundle{
			Urgent: EstimateLevel{
				MaxPriorityFeePerGas: est.Urgent.MaxPriorityFeePerGas.String(),
				MaxFeePerGas:         est.Urgent.MaxFeePerGas.String(),
				Confidence:           est.Urgent.Confidence,
			},
			Fast: EstimateLevel{
				MaxPriorityFeePerGas: est.Fast.MaxPriorityFeePerGas.String(),
				MaxFeePerGas:         est.Fast.MaxFeePerGas.String(),
				Confidence:           est.Fast.Confidence,
			},
			Standard: EstimateLevel{
				MaxPriorityFeePerGas: est.Standard.MaxPriorityFeePerGas.String(),
				MaxFeePerGas:         est.Standard.MaxFeePerGas.String(),
				Confidence:           est.Standard.Confidence,
			},
			Slow: EstimateLevel{
				MaxPriorityFeePerGas: est.Slow.MaxPriorityFeePerGas.String(),
				MaxFeePerGas:         est.Slow.MaxFeePerGas.String(),
				Confidence:           est.Slow.Confidence,
			},
		},
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(resp)
}

// handleStream provides server-sent events for estimate updates.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ctx := r.Context()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	var lastBlock uint64

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			est, err := s.provider.Current(ctx)
			if err != nil {
				continue
			}

			// Only send if block changed
			if est.BlockNumber == lastBlock {
				continue
			}
			lastBlock = est.BlockNumber

			data, _ := json.Marshal(map[string]any{
				"block_number": est.BlockNumber,
				"base_fee":     est.BaseFee.String(),
				"urgent":       est.Urgent.MaxPriorityFeePerGas.String(),
				"fast":         est.Fast.MaxPriorityFeePerGas.String(),
				"standard":     est.Standard.MaxPriorityFeePerGas.String(),
				"slow":         est.Slow.MaxPriorityFeePerGas.String(),
			})

			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}

func (s *Server) writeError(w http.ResponseWriter, status int, message string) {
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{
		"error": message,
	})
}
