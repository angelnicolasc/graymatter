package rpc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"time"
)

// DialContext opens a dedicated connection to a running daemon. The context
// bounds authentication and optional Ping as well as dialing. A successful
// connection remains usable after ctx is canceled and belongs to the caller.
// Use Dial for ordinary shared RPC clients.
func DialContext(ctx context.Context, opts DialOptions) (*Client, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := setDialDefaults(&opts); err != nil {
		return nil, err
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.DialTimeout+opts.CallTimeout)
		defer cancel()
	}

	addr, token, err := readDiscovery(opts.DataDir)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if err != nil {
		return nil, fmt.Errorf("rpc: read discovery: %w", err)
	}
	network, target, err := parseAddr(addr)
	if err != nil {
		return nil, err
	}

	dialCtx, cancelDial := context.WithTimeout(ctx, opts.DialTimeout)
	defer cancelDial()
	conn, err := (&net.Dialer{}).DialContext(dialCtx, network, target)
	if err != nil {
		return nil, fmt.Errorf("rpc: dial daemon: %w", err)
	}
	if err := authenticateContext(dialCtx, conn, token); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("rpc: authenticate daemon: %w", err)
	}

	c := newClient(conn, addr, opts)
	c.owned = true
	if opts.PingOnDial {
		if err := c.PingContext(ctx); err != nil {
			_ = c.Close()
			return nil, fmt.Errorf("rpc: ping after dial: %w", err)
		}
	}
	if err := ctx.Err(); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

func authenticateContext(ctx context.Context, conn net.Conn, token string) error {
	if err := ctx.Err(); err != nil || token == "" {
		return err
	}
	deadline, _ := ctx.Deadline()
	if err := conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	preamble := []byte(token + "\n")
	n, err := conn.Write(preamble)
	stop()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if err != nil {
		return err
	}
	if n != len(preamble) {
		return io.ErrShortWrite
	}
	return conn.SetWriteDeadline(time.Time{})
}

// PingContext verifies and records the capabilities of this exact dedicated
// connection. A successful Ping leaves it open for the report call.
func (c *Client) PingContext(ctx context.Context) error {
	if !c.owned {
		return errDedicatedRequired
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.callTimeout)
		defer cancel()
	}
	response, _, err := ownedCall[PingResponse](c, ctx, "Ping", &PingRequest{}, false)
	if err != nil {
		return err
	}
	if err := c.acceptPing(response); err != nil {
		_ = c.Close()
		return fmt.Errorf("rpc: invalid daemon handshake: %w", err)
	}
	return nil
}

var errDedicatedRequired = errors.New("rpc: a dedicated context connection is required")

const (
	pendingDispatch = iota
	potentialDispatch
	canceledBeforeDispatch
)

// ownedCall installs cancellation before net/rpc may block in Write. Only the
// worker accesses the response until the completion signal is received.
func ownedCall[T any](c *Client, ctx context.Context, method string, request any, mutation bool) (T, bool, error) {
	var zero T
	if err := ctx.Err(); err != nil {
		_ = c.Close()
		return zero, false, err
	}
	deadline, _ := ctx.Deadline()
	if err := c.conn.SetDeadline(deadline); err != nil {
		_ = c.Close()
		return zero, false, err
	}

	var dispatch atomic.Int32
	stop := context.AfterFunc(ctx, func() {
		dispatch.CompareAndSwap(pendingDispatch, canceledBeforeDispatch)
		_ = c.Close()
	})
	defer stop()
	response := new(T)
	done := make(chan error, 1)
	go func() {
		if err := ctx.Err(); err != nil {
			dispatch.CompareAndSwap(pendingDispatch, canceledBeforeDispatch)
			done <- err
			return
		}
		if !dispatch.CompareAndSwap(pendingDispatch, potentialDispatch) {
			done <- ctx.Err()
			return
		}
		done <- c.rpc.Call(ServiceName+"."+method, request, response)
	}()

	var err error
	select {
	case err = <-done:
	case <-ctx.Done():
		dispatch.CompareAndSwap(pendingDispatch, canceledBeforeDispatch)
		select {
		case err = <-done:
		default:
			_ = c.Close()
			return zero, dispatch.Load() == potentialDispatch, ctx.Err()
		}
	}
	stop()
	if err != nil {
		_ = c.Close()
		if ctxErr := ctx.Err(); ctxErr != nil {
			err = errors.Join(ctxErr, err)
		} else if !deadline.IsZero() && !time.Now().Before(deadline) {
			err = errors.Join(context.DeadlineExceeded, err)
		}
		return zero, dispatch.Load() == potentialDispatch, err
	}
	if !mutation {
		if err := ctx.Err(); err != nil {
			_ = c.Close()
			return zero, false, err
		}
		if err := c.conn.SetDeadline(time.Time{}); err != nil {
			_ = c.Close()
			return zero, false, err
		}
	}
	return *response, true, nil
}
