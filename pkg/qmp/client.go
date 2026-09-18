// Copyright 2026 The KubeVirt Metrics Exporter Authors
// SPDX-License-Identifier: Apache-2.0

package qmp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	libvirt "github.com/digitalocean/go-libvirt"
	"github.com/digitalocean/go-libvirt/socket/dialers"
)

const (
	qmpFlag               = 0
	defaultConnectTimeout = 5 * time.Second
)

// ErrClientClosed is returned when an operation is attempted on a closed Client.
// A closed client must not be reused; callers should reconnect.
var ErrClientClosed = errors.New("qmp client is closed")

type qmpCommand struct {
	Execute   string `json:"execute"`
	Arguments any    `json:"arguments,omitempty"`
}

type qmpResponse struct {
	Return json.RawMessage `json:"return"`
	Error  *struct {
		Class string `json:"class"`
		Desc  string `json:"desc"`
	} `json:"error,omitempty"`
}

type Client struct {
	mu     sync.Mutex // Serializes libvirt requests, but must not block Close.
	conn   net.Conn
	lv     *libvirt.Libvirt
	domain libvirt.Domain

	closed    atomic.Bool
	closeOnce sync.Once
	closeErr  error
}

// Dial connects to a domain with a bounded connection setup time.
func Dial(virtqemudSockPath, domainName string) (*Client, error) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultConnectTimeout)
	defer cancel()
	return DialContext(ctx, virtqemudSockPath, domainName)
}

// DialContext applies ctx to dialing, session setup, and domain lookup.
func DialContext(ctx context.Context, virtqemudSockPath, domainName string) (*Client, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", virtqemudSockPath)
	if err != nil {
		return nil, fmt.Errorf("dialing virtqemud at %s: %w", virtqemudSockPath, err)
	}
	return newClient(ctx, conn, domainName)
}

func newClient(ctx context.Context, conn net.Conn, domainName string) (*Client, error) {
	c := &Client{
		conn: conn,
		lv:   libvirt.NewWithDialer(dialers.NewAlreadyConnected(conn)),
	}
	err := c.call(ctx, func() error {
		if err := c.lv.ConnectToURI(libvirt.QEMUSession); err != nil {
			return fmt.Errorf("connecting to libvirt session: %w", err)
		}
		domain, err := c.lv.DomainLookupByName(domainName)
		if err != nil {
			return fmt.Errorf("looking up domain %s: %w", domainName, err)
		}
		c.domain = domain
		return nil
	})
	if err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// Close interrupts in-flight requests without waiting for a libvirt RPC.
// go-libvirt cleans up its reader and pending requests when the transport closes.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		c.closeErr = c.conn.Close()
	})
	return c.closeErr
}

// Closed reports whether Close has been called (including via context cancellation).
func (c *Client) Closed() bool {
	return c.closed.Load()
}

// call closes the transport on cancellation. Socket deadlines alone do not
// interrupt go-libvirt RPCs: its reader retries timeout errors while the caller
// remains blocked waiting for a response.
func (c *Client) call(ctx context.Context, fn func() error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.closed.Load() {
		return ErrClientClosed
	}

	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		c.Close()
		close(done)
	})
	err := fn()
	if !stop() {
		// Wait for the callback before another request can use this client.
		<-done
		return ctx.Err()
	}
	return err
}

func (c *Client) QueryBlockStats(ctx context.Context) (*BlockStatsResponse, error) {
	result, err := c.execQMP(ctx, "query-blockstats", nil)
	if err != nil {
		return nil, err
	}

	var devices []BlockDevice
	if err := json.Unmarshal(result, &devices); err != nil {
		return nil, fmt.Errorf("parsing blockstats response: %w", err)
	}

	return &BlockStatsResponse{Return: devices}, nil
}

func (c *Client) EnableHistogram(ctx context.Context, deviceID string, boundariesNs []int64) error {
	args := map[string]any{
		"id":         deviceID,
		"boundaries": boundariesNs,
	}
	_, err := c.execQMP(ctx, "block-latency-histogram-set", args)
	return err
}

func (c *Client) AgentCommand(ctx context.Context, cmd string, timeoutSec int32) (string, error) {
	var result []string
	err := c.call(ctx, func() error {
		var err error
		result, err = c.lv.QEMUDomainAgentCommand(c.domain, cmd, timeoutSec, 0)
		return err
	})
	if err != nil {
		return "", err
	}
	if len(result) == 0 {
		return "", fmt.Errorf("empty response from guest agent")
	}
	return result[0], nil
}

func (c *Client) QueryVirtio(ctx context.Context) ([]VirtioDevice, error) {
	result, err := c.execQMP(ctx, "x-query-virtio", nil)
	if err != nil {
		return nil, err
	}

	var devices []VirtioDevice
	if err := json.Unmarshal(result, &devices); err != nil {
		return nil, fmt.Errorf("parsing x-query-virtio response: %w", err)
	}

	return devices, nil
}

func (c *Client) QueryVirtioStatus(ctx context.Context, path string) (*VirtioStatus, error) {
	args := map[string]any{"path": path}
	result, err := c.execQMP(ctx, "x-query-virtio-status", args)
	if err != nil {
		return nil, err
	}

	var status VirtioStatus
	if err := json.Unmarshal(result, &status); err != nil {
		return nil, fmt.Errorf("parsing x-query-virtio-status response: %w", err)
	}

	return &status, nil
}

func (c *Client) QueryVirtioQueueStatus(ctx context.Context, path string, queue int) (*VirtQueueStatus, error) {
	args := map[string]any{"path": path, "queue": queue}
	result, err := c.execQMP(ctx, "x-query-virtio-queue-status", args)
	if err != nil {
		return nil, err
	}

	var status VirtQueueStatus
	if err := json.Unmarshal(result, &status); err != nil {
		return nil, fmt.Errorf("parsing x-query-virtio-queue-status response: %w", err)
	}

	return &status, nil
}

// DomainGetXMLDesc returns the libvirt domain XML for the connected domain.
func (c *Client) DomainGetXMLDesc() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultConnectTimeout)
	defer cancel()
	var result string
	err := c.call(ctx, func() error {
		var err error
		result, err = c.lv.DomainGetXMLDesc(c.domain, 0)
		return err
	})
	return result, err
}

func (c *Client) execQMP(ctx context.Context, command string, args any) (json.RawMessage, error) {
	cmd := qmpCommand{Execute: command, Arguments: args}
	cmdJSON, err := json.Marshal(cmd)
	if err != nil {
		return nil, fmt.Errorf("marshaling QMP command: %w", err)
	}

	var result string
	err = c.call(ctx, func() error {
		var err error
		result, err = c.lv.QEMUDomainMonitorCommand(c.domain, string(cmdJSON), uint32(qmpFlag))
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("QMP %s: %w", command, err)
	}

	var resp qmpResponse
	if err := json.Unmarshal([]byte(result), &resp); err != nil {
		return nil, fmt.Errorf("parsing QMP response for %s: %w", command, err)
	}

	if resp.Error != nil {
		return nil, fmt.Errorf("QMP %s error: %s: %s", command, resp.Error.Class, resp.Error.Desc)
	}

	return resp.Return, nil
}
