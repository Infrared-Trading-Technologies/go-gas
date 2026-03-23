package eth

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/goccy/go-json"
	"github.com/holiman/uint256"
)

// BlockReader abstracts block fetching operations.
type BlockReader interface {
	BlockByNumber(ctx context.Context, number *uint256.Int) (*Block, error)
	LatestBlock(ctx context.Context) (*Block, error)
	ChainID(ctx context.Context) (uint64, error)
}

// FeeReader abstracts gas fee queries.
type FeeReader interface {
	// MaxPriorityFeePerGas returns the node's suggested priority fee via eth_maxPriorityFeePerGas.
	MaxPriorityFeePerGas(ctx context.Context) (*uint256.Int, error)
}

// TxPoolReader abstracts mempool access.
type TxPoolReader interface {
	PendingTransactions(ctx context.Context, limit int) ([]*Transaction, error)
}

// TransactionReader abstracts transaction fetching.
type TransactionReader interface {
	TransactionByHash(ctx context.Context, hash string) (*Transaction, error)
	TransactionsByHashes(ctx context.Context, hashes []string) ([]*Transaction, error)
}

// Client provides access to an Ethereum node via JSON-RPC.
type Client struct {
	httpURL    string
	httpClient *http.Client
	requestID  atomic.Uint64
}

// NewClient creates a new Ethereum RPC client.
func NewClient(httpURL string) *Client {
	return &Client{
		httpURL: httpURL,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        1000,
				MaxIdleConnsPerHost: 1000,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

// ChainID returns the chain ID of the connected network.
func (c *Client) ChainID(ctx context.Context) (uint64, error) {
	var result hexUint64
	if err := c.call(ctx, "eth_chainId", nil, &result); err != nil {
		return 0, err
	}
	return uint64(result), nil
}

// MaxPriorityFeePerGas returns the node's suggested priority fee via eth_maxPriorityFeePerGas.
func (c *Client) MaxPriorityFeePerGas(ctx context.Context) (*uint256.Int, error) {
	var result hexUint64
	if err := c.call(ctx, "eth_maxPriorityFeePerGas", nil, &result); err != nil {
		return nil, err
	}
	return uint256.NewInt(uint64(result)), nil
}

// LatestBlock returns the most recent block.
func (c *Client) LatestBlock(ctx context.Context) (*Block, error) {
	return c.blockByTag(ctx, "latest", true)
}

// BlockByNumber returns the block at the given height.
// Pass nil for the latest block.
func (c *Client) BlockByNumber(ctx context.Context, number *uint256.Int) (*Block, error) {
	if number == nil {
		return c.LatestBlock(ctx)
	}
	tag := number.Hex()
	return c.blockByTag(ctx, tag, true)
}

func (c *Client) blockByTag(ctx context.Context, tag string, includeTxs bool) (*Block, error) {
	var raw rpcBlock
	if err := c.call(ctx, "eth_getBlockByNumber", []any{tag, includeTxs}, &raw); err != nil {
		return nil, err
	}
	return raw.toBlock(includeTxs)
}

// TransactionByHash returns the transaction with the given hash.
func (c *Client) TransactionByHash(ctx context.Context, hash string) (*Transaction, error) {
	var raw rpcTransaction
	if err := c.call(ctx, "eth_getTransactionByHash", []any{hash}, &raw); err != nil {
		return nil, err
	}
	tx := raw.toTransaction()
	return &tx, nil
}

// TransactionsByHashes fetches multiple transactions in a single batch request.
func (c *Client) TransactionsByHashes(ctx context.Context, hashes []string) ([]*Transaction, error) {
	if len(hashes) == 0 {
		return nil, nil
	}

	reqs := make([]rpcRequest, len(hashes))
	for i, hash := range hashes {
		reqs[i] = rpcRequest{
			JSONRPC: "2.0",
			ID:      c.requestID.Add(1),
			Method:  "eth_getTransactionByHash",
			Params:  []any{hash},
		}
	}

	responses, err := c.batchCall(ctx, reqs)
	if err != nil {
		return nil, err
	}

	txs := make([]*Transaction, 0, len(responses))
	for _, resp := range responses {
		if resp.Error != nil {
			// Log error or skip? For now, skip failed lookups
			continue
		}
		if len(resp.Result) == 0 || string(resp.Result) == "null" {
			continue
		}

		var raw rpcTransaction
		if err := json.Unmarshal(resp.Result, &raw); err != nil {
			continue
		}
		tx := raw.toTransaction()
		txs = append(txs, &tx)
	}

	return txs, nil
}

// PendingTransactions returns pending transactions from the mempool.
// Uses txpool_content and samples up to limit transactions.
//
// CRITICAL WARNING: This method uses the `txpool_content` RPC method which fetches
// the ENTIRE mempool content. On high-traffic chains (like Mainnet), this payload
// can be 100MB+ and take seconds to transfer/parse. This is NOT suitable for
// "ultra low latency" in production.
//
// TODO(optimization): Replace with:
// 1. WebSocket subscription to `newPendingTransactions` (eth_subscribe).
// 2. `eth_newPendingTransactionFilter` + `eth_getFilterChanges` (polling hashes).
// 3. A specialized mempool service or node plugin.
func (c *Client) PendingTransactions(ctx context.Context, limit int) ([]*Transaction, error) {
	var result struct {
		Pending map[string]map[string]rpcTransaction `json:"pending"`
	}

	if err := c.call(ctx, "txpool_content", nil, &result); err != nil {
		// Fall back to eth_pendingTransactions if txpool_content not available
		return c.pendingTransactionsFallback(ctx, limit)
	}

	var txs []*Transaction
	for _, nonces := range result.Pending {
		for _, rtx := range nonces {
			tx := rtx.toTransaction()
			txs = append(txs, &tx)
			if len(txs) >= limit {
				return txs, nil
			}
		}
	}

	return txs, nil
}

func (c *Client) pendingTransactionsFallback(ctx context.Context, limit int) ([]*Transaction, error) {
	var raw []rpcTransaction
	if err := c.call(ctx, "eth_pendingTransactions", nil, &raw); err != nil {
		return nil, fmt.Errorf("eth_pendingTransactions: %w", err)
	}

	txs := make([]*Transaction, 0, min(len(raw), limit))
	for i := 0; i < len(raw) && i < limit; i++ {
		tx := raw[i].toTransaction()
		txs = append(txs, &tx)
	}

	return txs, nil
}

// Close releases resources. Currently a no-op for HTTP client.
func (c *Client) Close() error {
	c.httpClient.CloseIdleConnections()
	return nil
}

// rpcRequest represents a JSON-RPC request.
type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      uint64 `json:"id"`
	Method  string `json:"method"`
	Params  []any  `json:"params,omitempty"`
}

// rpcResponse represents a JSON-RPC response.
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      uint64          `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message)
}

func (c *Client) call(ctx context.Context, method string, params []any, result any) error {
	req := rpcRequest{
		JSONRPC: "2.0",
		ID:      c.requestID.Add(1),
		Method:  method,
		Params:  params,
	}

	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshaling request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.httpURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}

	var rpcResp rpcResponse
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}

	if rpcResp.Error != nil {
		return rpcResp.Error
	}

	if result != nil {
		if err := json.Unmarshal(rpcResp.Result, result); err != nil {
			return fmt.Errorf("unmarshaling result: %w", err)
		}
	}

	return nil
}

func (c *Client) batchCall(ctx context.Context, reqs []rpcRequest) ([]rpcResponse, error) {
	body, err := json.Marshal(reqs)
	if err != nil {
		return nil, fmt.Errorf("marshaling batch request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.httpURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating batch request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("sending batch request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}

	var rpcResps []rpcResponse
	if err := json.NewDecoder(resp.Body).Decode(&rpcResps); err != nil {
		return nil, fmt.Errorf("decoding batch response: %w", err)
	}

	return rpcResps, nil
}
