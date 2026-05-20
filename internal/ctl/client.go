package ctl

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"
)

var dialProbeTimeout = 300 * time.Millisecond

// Client is the short-lived counterpart to Server: one Call per connection.
type Client struct {
	socket string
}

func NewClient(socket string) *Client { return &Client{socket: socket} }

// Call sends one request and returns the raw response. The caller decodes Data.
func (c *Client) Call(ctx context.Context, method string, args any) (*Response, error) {
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "unix", c.socket)
	if err != nil {
		return nil, &NotRunningError{Socket: c.socket, Cause: err}
	}
	defer conn.Close()

	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}

	var raw json.RawMessage
	if args != nil {
		b, err := json.Marshal(args)
		if err != nil {
			return nil, fmt.Errorf("encode args: %w", err)
		}
		raw = b
	}
	req := Request{Method: method, Args: raw}
	if err := json.NewEncoder(conn).Encode(&req); err != nil {
		return nil, fmt.Errorf("write request: %w", err)
	}
	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return nil, fmt.Errorf("read response: %w", err)
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	return &resp, nil
}

// NotRunningError is the dial-failure surfaced to the CLI so it can exit 3.
type NotRunningError struct {
	Socket string
	Cause  error
}

func (e *NotRunningError) Error() string {
	return fmt.Sprintf("outpost daemon is not running (socket %s: %v)", e.Socket, e.Cause)
}

func (e *NotRunningError) Unwrap() error { return e.Cause }

// IsNotRunning reports whether err is a NotRunningError.
func IsNotRunning(err error) bool {
	var n *NotRunningError
	return errors.As(err, &n)
}
