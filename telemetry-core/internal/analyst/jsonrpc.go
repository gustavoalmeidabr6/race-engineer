package analyst

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

// jsonrpc.go implements just enough of JSON-RPC 2.0 over newline-delimited
// JSON to drive the Agent Client Protocol. The wire framing is one JSON
// object per line on each direction's pipe:
//
//	Go → opencode (stdin):  {"jsonrpc":"2.0","id":N,"method":"...","params":...}
//	opencode → Go (stdout): {"jsonrpc":"2.0","id":N,"result":...} (response)
//	                       {"jsonrpc":"2.0","method":"session/update","params":...} (notification)
//	                       {"jsonrpc":"2.0","id":N,"method":"fs/...","params":...} (server-initiated request)
//
// This implementation supports both directions: client-initiated requests
// (where we wait for a response keyed by id) and server-initiated requests
// (where the registered handler returns a result the conn writes back).

const jsonrpcVersion = "2.0"

// rpcMessage is the loose schema that covers every direction. The id field is
// json.RawMessage so we can distinguish "no id" (notification) from "id is 0".
type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	if e == nil {
		return ""
	}
	if len(e.Data) > 0 {
		return fmt.Sprintf("rpc error %d: %s (%s)", e.Code, e.Message, string(e.Data))
	}
	return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message)
}

// ServerRequestHandler is invoked when the peer sends a request (id present,
// method set). Returning a non-nil error sends an rpc error response.
type ServerRequestHandler func(ctx context.Context, method string, params json.RawMessage) (any, error)

// NotificationHandler is invoked for incoming notifications (method set, no id).
type NotificationHandler func(method string, params json.RawMessage)

// Conn is a bidirectional JSON-RPC 2.0 connection over a reader/writer pair.
type Conn struct {
	r io.ReadCloser
	w io.WriteCloser

	writeMu sync.Mutex

	pendingMu sync.Mutex
	pending   map[int64]chan rpcMessage
	nextID    atomic.Int64

	onRequest      ServerRequestHandler
	onNotification NotificationHandler

	closeOnce sync.Once
	closed    chan struct{}
	loopErr   error
}

// NewConn wires a Conn around the given pipes. Call Loop in a goroutine to
// pump incoming frames; Loop returns when the reader hits EOF or any frame
// fails to parse.
func NewConn(r io.ReadCloser, w io.WriteCloser, onRequest ServerRequestHandler, onNotification NotificationHandler) *Conn {
	return &Conn{
		r:              r,
		w:              w,
		pending:        map[int64]chan rpcMessage{},
		onRequest:      onRequest,
		onNotification: onNotification,
		closed:         make(chan struct{}),
	}
}

// Close shuts down the conn. Outstanding Call invocations unblock with
// io.ErrClosedPipe.
func (c *Conn) Close() error {
	var firstErr error
	c.closeOnce.Do(func() {
		close(c.closed)
		if err := c.r.Close(); err != nil && !errors.Is(err, io.ErrClosedPipe) {
			firstErr = err
		}
		if err := c.w.Close(); err != nil && firstErr == nil && !errors.Is(err, io.ErrClosedPipe) {
			firstErr = err
		}
		c.pendingMu.Lock()
		for id, ch := range c.pending {
			close(ch)
			delete(c.pending, id)
		}
		c.pendingMu.Unlock()
	})
	return firstErr
}

// Done returns a channel closed when the conn shuts down (after Close, or
// after Loop exits on its own).
func (c *Conn) Done() <-chan struct{} { return c.closed }

// LoopErr is the read-loop's final error (set after Done fires). nil for a
// clean EOF.
func (c *Conn) LoopErr() error { return c.loopErr }

// Loop reads NDJSON frames until the reader returns an error. Each frame is
// routed: id+result → pending Call; id+method → onRequest; method only →
// onNotification.
func (c *Conn) Loop(ctx context.Context) {
	scanner := bufio.NewScanner(c.r)
	// opencode emits large agent_message_chunk payloads — bump the line
	// budget to 8 MiB to keep them in one frame.
	buf := make([]byte, 0, 256*1024)
	scanner.Buffer(buf, 8*1024*1024)

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			c.loopErr = ctx.Err()
			c.Close()
			return
		default:
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var msg rpcMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			// Don't kill the conn over one malformed line — opencode may
			// occasionally log to stdout. Keep reading.
			continue
		}
		c.dispatch(ctx, msg)
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		c.loopErr = err
	}
	c.Close()
}

func (c *Conn) dispatch(ctx context.Context, msg rpcMessage) {
	// Response to one of our outbound Calls: id present, no method.
	if msg.Method == "" && len(msg.ID) > 0 {
		var id int64
		if err := json.Unmarshal(msg.ID, &id); err != nil {
			return
		}
		c.pendingMu.Lock()
		ch, ok := c.pending[id]
		if ok {
			delete(c.pending, id)
		}
		c.pendingMu.Unlock()
		if ok {
			ch <- msg
			close(ch)
		}
		return
	}
	// Server-initiated request: id present AND method set.
	if msg.Method != "" && len(msg.ID) > 0 {
		go c.handleRequest(ctx, msg)
		return
	}
	// Notification: method set, no id.
	if msg.Method != "" {
		if c.onNotification != nil {
			c.onNotification(msg.Method, msg.Params)
		}
		return
	}
}

func (c *Conn) handleRequest(ctx context.Context, msg rpcMessage) {
	if c.onRequest == nil {
		c.writeError(msg.ID, -32601, "method not found: "+msg.Method)
		return
	}
	result, err := c.onRequest(ctx, msg.Method, msg.Params)
	if err != nil {
		c.writeError(msg.ID, -32000, err.Error())
		return
	}
	raw, mErr := json.Marshal(result)
	if mErr != nil {
		c.writeError(msg.ID, -32603, "marshal result: "+mErr.Error())
		return
	}
	c.writeRaw(rpcMessage{JSONRPC: jsonrpcVersion, ID: msg.ID, Result: raw})
}

// Call sends a client-initiated request and blocks until a response arrives
// (or ctx fires). Errors include io errors writing to the pipe, ctx
// cancellation, and rpc errors returned by the peer.
func (c *Conn) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := c.nextID.Add(1)
	idRaw, _ := json.Marshal(id)

	var paramsRaw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("marshal params: %w", err)
		}
		paramsRaw = b
	}

	ch := make(chan rpcMessage, 1)
	c.pendingMu.Lock()
	c.pending[id] = ch
	c.pendingMu.Unlock()

	if err := c.writeRaw(rpcMessage{
		JSONRPC: jsonrpcVersion,
		ID:      idRaw,
		Method:  method,
		Params:  paramsRaw,
	}); err != nil {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return nil, err
	}

	select {
	case msg, ok := <-ch:
		if !ok {
			return nil, io.ErrClosedPipe
		}
		if msg.Error != nil {
			return nil, msg.Error
		}
		return msg.Result, nil
	case <-ctx.Done():
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return nil, ctx.Err()
	case <-c.closed:
		return nil, io.ErrClosedPipe
	}
}

// Notify sends a one-way notification (no id, no response expected).
func (c *Conn) Notify(method string, params any) error {
	var paramsRaw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return fmt.Errorf("marshal params: %w", err)
		}
		paramsRaw = b
	}
	return c.writeRaw(rpcMessage{JSONRPC: jsonrpcVersion, Method: method, Params: paramsRaw})
}

func (c *Conn) writeError(id json.RawMessage, code int, message string) {
	c.writeRaw(rpcMessage{
		JSONRPC: jsonrpcVersion,
		ID:      id,
		Error:   &rpcError{Code: code, Message: message},
	})
}

func (c *Conn) writeRaw(msg rpcMessage) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	select {
	case <-c.closed:
		return io.ErrClosedPipe
	default:
	}
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = c.w.Write(b)
	return err
}
