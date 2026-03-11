package eth

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goccy/go-json"
)

// Subscriber manages WebSocket subscriptions to an Ethereum node.
type Subscriber interface {
	SubscribeNewHeads(ctx context.Context) (<-chan *Block, error)
	SubscribeNewPendingTransactions(ctx context.Context) (<-chan string, error)
	Close() error
}

// WSSubscriber implements Subscriber using WebSocket connections.
type WSSubscriber struct {
	wsURL  string
	logger *slog.Logger

	mu       sync.Mutex
	conn     net.Conn
	reader   *bufio.Reader
	subs     map[string]chan json.RawMessage
	closed   atomic.Bool
	done     chan struct{}
	subCount atomic.Uint64
	writeMu  sync.Mutex
}

// NewWSSubscriber creates a new WebSocket subscriber.
func NewWSSubscriber(wsURL string, logger *slog.Logger) *WSSubscriber {
	return &WSSubscriber{
		wsURL:  wsURL,
		logger: logger,
		subs:   make(map[string]chan json.RawMessage),
		done:   make(chan struct{}),
	}
}

// Connect establishes the WebSocket connection.
func (s *WSSubscriber) Connect(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed.Load() {
		return errors.New("subscriber closed")
	}

	u, err := url.Parse(s.wsURL)
	if err != nil {
		return fmt.Errorf("parsing URL: %w", err)
	}

	host := u.Host
	if u.Port() == "" {
		if u.Scheme == "wss" {
			host = host + ":443"
		} else {
			host = host + ":80"
		}
	}

	var conn net.Conn
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	conn, err = dialer.DialContext(ctx, "tcp", host)
	if err != nil {
		return fmt.Errorf("dialing: %w", err)
	}

	// Handle WSS (TLS)
	if u.Scheme == "wss" {
		tlsConfig := &tls.Config{
			ServerName: u.Hostname(),
		}
		tlsConn := tls.Client(conn, tlsConfig)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			conn.Close()
			return fmt.Errorf("tls handshake: %w", err)
		}
		conn = tlsConn
	}

	// Perform WebSocket handshake
	key := make([]byte, 16)
	if _, err := rand.Read(key); err != nil {
		conn.Close()
		return fmt.Errorf("generating key: %w", err)
	}
	wsKey := base64.StdEncoding.EncodeToString(key)

	path := u.Path
	if path == "" {
		path = "/"
	}
	if u.RawQuery != "" {
		path += "?" + u.RawQuery
	}

	req := fmt.Sprintf("GET %s HTTP/1.1\r\n"+
		"Host: %s\r\n"+
		"Upgrade: websocket\r\n"+
		"Connection: Upgrade\r\n"+
		"Sec-WebSocket-Key: %s\r\n"+
		"Sec-WebSocket-Version: 13\r\n"+
		"\r\n", path, u.Host, wsKey)

	if _, err := conn.Write([]byte(req)); err != nil {
		conn.Close()
		return fmt.Errorf("sending handshake: %w", err)
	}

	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		conn.Close()
		return fmt.Errorf("reading handshake response: %w", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusSwitchingProtocols {
		conn.Close()
		return fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}

	// Verify Sec-WebSocket-Accept
	h := sha1.New()
	h.Write([]byte(wsKey + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	expectedAccept := base64.StdEncoding.EncodeToString(h.Sum(nil))
	if resp.Header.Get("Sec-WebSocket-Accept") != expectedAccept {
		conn.Close()
		return errors.New("invalid Sec-WebSocket-Accept")
	}

	s.conn = conn
	s.reader = reader

	go s.readLoop()

	s.logger.Info("websocket connected", "url", s.wsURL)
	return nil
}

// SubscribeNewPendingTransactions subscribes to new pending transaction hashes.
func (s *WSSubscriber) SubscribeNewPendingTransactions(ctx context.Context) (<-chan string, error) {
	s.mu.Lock()
	needsConnect := s.conn == nil
	s.mu.Unlock()

	if needsConnect {
		if err := s.Connect(ctx); err != nil {
			return nil, err
		}
	}

	subID, rawCh, err := s.subscribe(ctx, "newPendingTransactions")
	if err != nil {
		return nil, fmt.Errorf("subscribing to newPendingTransactions: %w", err)
	}

	txHashCh := make(chan string, 128)

	go func() {
		defer close(txHashCh)
		defer s.unsubscribe(subID)

		for {
			select {
			case <-ctx.Done():
				return
			case <-s.done:
				return
			case raw, ok := <-rawCh:
				if !ok {
					return
				}
				var txHash string
				if err := json.Unmarshal(raw, &txHash); err != nil {
					s.logger.Error("parsing tx hash", "error", err)
					continue
				}
				select {
				case txHashCh <- txHash:
				default:
					// Drop if buffer full - we only need a sample
				}
			}
		}
	}()

	return txHashCh, nil
}

// SubscribeNewHeads subscribes to new block headers.
func (s *WSSubscriber) SubscribeNewHeads(ctx context.Context) (<-chan *Block, error) {
	s.mu.Lock()
	needsConnect := s.conn == nil
	s.mu.Unlock()

	if needsConnect {
		if err := s.Connect(ctx); err != nil {
			return nil, err
		}
	}

	subID, rawCh, err := s.subscribe(ctx, "newHeads")
	if err != nil {
		return nil, fmt.Errorf("subscribing to newHeads: %w", err)
	}

	blockCh := make(chan *Block, 16)

	go func() {
		defer close(blockCh)
		defer s.unsubscribe(subID)

		for {
			select {
			case <-ctx.Done():
				return
			case <-s.done:
				return
			case raw, ok := <-rawCh:
				if !ok {
					return
				}
				block, err := s.parseBlockHeader(raw)
				if err != nil {
					s.logger.Error("parsing block header", "error", err)
					continue
				}
				select {
				case blockCh <- block:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	return blockCh, nil
}

func (s *WSSubscriber) subscribe(ctx context.Context, event string) (string, chan json.RawMessage, error) {
	id := s.subCount.Add(1)

	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "eth_subscribe",
		"params":  []string{event},
	}

	respCh := make(chan json.RawMessage, 1)

	// Temporarily store to receive the response
	s.mu.Lock()
	tempID := fmt.Sprintf("temp_%d", id)
	s.subs[tempID] = respCh
	s.mu.Unlock()

	if err := s.writeJSON(req); err != nil {
		s.mu.Lock()
		delete(s.subs, tempID)
		s.mu.Unlock()
		return "", nil, fmt.Errorf("sending subscribe request: %w", err)
	}

	// Wait for response with timeout
	select {
	case <-ctx.Done():
		s.mu.Lock()
		delete(s.subs, tempID)
		s.mu.Unlock()
		return "", nil, ctx.Err()
	case <-time.After(10 * time.Second):
		s.mu.Lock()
		delete(s.subs, tempID)
		s.mu.Unlock()
		return "", nil, errors.New("subscription timeout")
	case raw := <-respCh:
		s.mu.Lock()
		delete(s.subs, tempID)
		s.mu.Unlock()

		var resp struct {
			Result string `json:"result"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(raw, &resp); err != nil {
			return "", nil, fmt.Errorf("parsing subscribe response: %w", err)
		}
		if resp.Error != nil {
			return "", nil, fmt.Errorf("subscription error: %s", resp.Error.Message)
		}

		subID := resp.Result
		ch := make(chan json.RawMessage, 64)
		s.mu.Lock()
		s.subs[subID] = ch
		s.mu.Unlock()

		s.logger.Debug("subscribed", "event", event, "subscription_id", subID)
		return subID, ch, nil
	}
}

func (s *WSSubscriber) unsubscribe(subID string) {
	s.mu.Lock()
	if ch, ok := s.subs[subID]; ok {
		close(ch)
		delete(s.subs, subID)
	}
	s.mu.Unlock()

	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      s.subCount.Add(1),
		"method":  "eth_unsubscribe",
		"params":  []string{subID},
	}
	_ = s.writeJSON(req)
}

func (s *WSSubscriber) readLoop() {
	defer func() {
		s.mu.Lock()
		for _, ch := range s.subs {
			close(ch)
		}
		s.subs = make(map[string]chan json.RawMessage)
		if s.conn != nil {
			s.conn.Close()
			s.conn = nil
		}
		s.mu.Unlock()
	}()

	var (
		lastDropLog  time.Time
		droppedCount int64
	)

	for {
		select {
		case <-s.done:
			return
		default:
		}

		s.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		data, err := s.readFrame()
		if err != nil {
			if !s.closed.Load() {
				s.logger.Error("websocket read error", "error", err)
			}
			return
		}

		// Try to parse as subscription notification
		var notification struct {
			ID      uint64 `json:"id"`
			JSONRPC string `json:"jsonrpc"`
			Method  string `json:"method"`
			Params  struct {
				Subscription string          `json:"subscription"`
				Result       json.RawMessage `json:"result"`
			} `json:"params"`
			Result json.RawMessage `json:"result"`
		}

		if err := json.Unmarshal(data, &notification); err != nil {
			s.logger.Warn("failed to parse message", "error", err)
			continue
		}

		s.mu.Lock()
		if notification.Method == "eth_subscription" {
			// Subscription notification
			if ch, ok := s.subs[notification.Params.Subscription]; ok {
				select {
				case ch <- notification.Params.Result:
				default:
					droppedCount++
					now := time.Now()
					if now.Sub(lastDropLog) >= 10*time.Second {
						s.logger.Warn("subscription channel full, dropping messages",
							"subscription_id", notification.Params.Subscription,
							"dropped_count", droppedCount)
						lastDropLog = now
						droppedCount = 0
					}
				}
			}
		} else if notification.ID > 0 {
			// RPC response - route to temp subscription
			tempID := fmt.Sprintf("temp_%d", notification.ID)
			if ch, ok := s.subs[tempID]; ok {
				select {
				case ch <- data:
				default:
				}
			}
		}
		s.mu.Unlock()
	}
}

func (s *WSSubscriber) writeJSON(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.writeFrame(data)
}

func (s *WSSubscriber) writeFrame(data []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()

	if conn == nil {
		return fmt.Errorf("connection closed")
	}

	// WebSocket frame: FIN=1, opcode=1 (text), mask=1 (client must mask)
	frame := make([]byte, 0, 14+len(data))
	frame = append(frame, 0x81) // FIN + text frame

	// Payload length
	if len(data) < 126 {
		frame = append(frame, byte(len(data))|0x80) // Set mask bit
	} else if len(data) < 65536 {
		frame = append(frame, 126|0x80)
		frame = append(frame, byte(len(data)>>8), byte(len(data)))
	} else {
		frame = append(frame, 127|0x80)
		frame = append(frame, make([]byte, 8)...)
		binary.BigEndian.PutUint64(frame[len(frame)-8:], uint64(len(data)))
	}

	// Masking key
	mask := make([]byte, 4)
	if _, err := rand.Read(mask); err != nil {
		return err
	}
	frame = append(frame, mask...)

	// Masked payload
	masked := make([]byte, len(data))
	for i, b := range data {
		masked[i] = b ^ mask[i%4]
	}
	frame = append(frame, masked...)

	_, err := conn.Write(frame)
	return err
}

func (s *WSSubscriber) readFrame() ([]byte, error) {
	for {
		// Read first 2 bytes
		header := make([]byte, 2)
		if _, err := io.ReadFull(s.reader, header); err != nil {
			return nil, err
		}

		// Check opcode
		opcode := header[0] & 0x0F

		// Payload length
		payloadLen := int64(header[1] & 0x7F)
		if payloadLen == 126 {
			ext := make([]byte, 2)
			if _, err := io.ReadFull(s.reader, ext); err != nil {
				return nil, err
			}
			payloadLen = int64(binary.BigEndian.Uint16(ext))
		} else if payloadLen == 127 {
			ext := make([]byte, 8)
			if _, err := io.ReadFull(s.reader, ext); err != nil {
				return nil, err
			}
			payloadLen = int64(binary.BigEndian.Uint64(ext))
		}

		// Check for mask (server should not mask, but we should handle skipping it if present)
		if header[1]&0x80 != 0 {
			// Skip mask key
			mask := make([]byte, 4)
			if _, err := io.ReadFull(s.reader, mask); err != nil {
				return nil, err
			}
		}

		// Read payload
		payload := make([]byte, payloadLen)
		if _, err := io.ReadFull(s.reader, payload); err != nil {
			return nil, err
		}

		switch opcode {
		case 0x01, 0x02: // Text or Binary
			return payload, nil
		case 0x08: // Close
			return nil, errors.New("connection closed by server")
		case 0x09: // Ping
			s.logger.Debug("received ping, sending pong")
			if err := s.writePong(payload); err != nil {
				return nil, fmt.Errorf("sending pong: %w", err)
			}
			continue // Read next frame
		case 0x0A: // Pong
			s.logger.Debug("received pong")
			continue // Read next frame
		default:
			// Ignore unknown opcodes
			continue
		}
	}
}

func (s *WSSubscriber) writePong(data []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()

	if conn == nil {
		return fmt.Errorf("connection closed")
	}

	// WebSocket frame: FIN=1, opcode=0xA (Pong), mask=1
	frame := make([]byte, 0, 14+len(data))
	frame = append(frame, 0x8A) // FIN + Pong

	// Payload length
	if len(data) < 126 {
		frame = append(frame, byte(len(data))|0x80) // Set mask bit
	} else if len(data) < 65536 {
		frame = append(frame, 126|0x80)
		frame = append(frame, byte(len(data)>>8), byte(len(data)))
	} else {
		frame = append(frame, 127|0x80)
		frame = append(frame, make([]byte, 8)...)
		binary.BigEndian.PutUint64(frame[len(frame)-8:], uint64(len(data)))
	}

	// Masking key
	mask := make([]byte, 4)
	if _, err := rand.Read(mask); err != nil {
		return err
	}
	frame = append(frame, mask...)

	// Masked payload
	masked := make([]byte, len(data))
	for i, b := range data {
		masked[i] = b ^ mask[i%4]
	}
	frame = append(frame, masked...)

	_, err := conn.Write(frame)
	return err
}

func (s *WSSubscriber) parseBlockHeader(raw json.RawMessage) (*Block, error) {
	var header rpcBlock
	if err := json.Unmarshal(raw, &header); err != nil {
		return nil, err
	}
	return header.toBlock(false)
}

// Close shuts down the subscriber and all active subscriptions.
func (s *WSSubscriber) Close() error {
	if s.closed.Swap(true) {
		return nil
	}

	close(s.done)

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.conn != nil {
		// Send close frame
		s.writeMu.Lock()
		closeFrame := []byte{0x88, 0x02, 0x03, 0xe8} // Close with 1000 (normal closure)
		s.conn.Write(closeFrame)
		s.writeMu.Unlock()
		return s.conn.Close()
	}
	return nil
}
