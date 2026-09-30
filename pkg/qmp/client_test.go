// Copyright 2026 The KubeVirt Metrics Exporter Authors
// SPDX-License-Identifier: Apache-2.0

package qmp

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// mockLibvirt serves the actual libvirt wire protocol over an in-memory pipe.
// A nil reply leaves the request unanswered, as with an unresponsive daemon.
func mockLibvirt(reply func(int) []byte) (net.Conn, <-chan int) {
	GinkgoHelper()
	client, server := net.Pipe()
	requests := make(chan int, 32)
	done := make(chan struct{})
	go func() {
		defer GinkgoRecover()
		defer close(done)
		defer server.Close()
		for i := 1; ; i++ {
			var length uint32
			if err := binary.Read(server, binary.BigEndian, &length); err != nil {
				return
			}
			Expect(length).To(BeNumerically(">=", 28), "invalid libvirt frame length")
			Expect(length).To(BeNumerically("<=", 1024*1024), "invalid libvirt frame length")
			frame := make([]byte, length-4)
			if _, err := io.ReadFull(server, frame); err != nil {
				return
			}
			requests <- i
			payload := reply(i)
			if payload == nil {
				continue
			}
			response := make([]byte, 28+len(payload))
			binary.BigEndian.PutUint32(response[:4], uint32(len(response)))
			copy(response[4:28], frame[:24])
			binary.BigEndian.PutUint32(response[16:20], 1) // RPC reply
			copy(response[28:], payload)
			if _, err := server.Write(response); err != nil {
				return
			}
		}
	}()
	DeferCleanup(func() {
		client.Close()
		server.Close()
		Eventually(done, 2*time.Second).Should(BeClosed(), "mock libvirt server did not stop")
	})
	return client, requests
}

func xdrString(value string) []byte {
	b := make([]byte, 4+(len(value)+3)/4*4)
	binary.BigEndian.PutUint32(b[:4], uint32(len(value)))
	copy(b[4:], value)
	return b
}

func setupReply(i int) []byte {
	switch i {
	case 1: // auth-list: no authentication required
		return make([]byte, 4)
	case 2: // connect-open: void
		return []byte{}
	case 3: // domain-lookup: name, UUID, ID
		return append(xdrString("test-vm"), make([]byte, 20)...)
	default:
		return nil
	}
}

func connectedClient(reply func(int) []byte) (*Client, <-chan int) {
	GinkgoHelper()
	conn, requests := mockLibvirt(func(i int) []byte {
		if i <= 3 {
			return setupReply(i)
		}
		return reply(i)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, err := newClient(ctx, conn, "test-vm")
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() { c.Close() })
	for i := 0; i < 3; i++ {
		<-requests
	}
	return c, requests
}

func awaitRequest(requests <-chan int) {
	GinkgoHelper()
	Eventually(requests, 2*time.Second).Should(Receive(), "libvirt request was not sent")
}

func awaitError(done <-chan error) error {
	GinkgoHelper()
	var err error
	Eventually(done, 2*time.Second).Should(Receive(&err), "libvirt call remained blocked")
	return err
}

var _ = Describe("QMP client cancellation", func() {
	DescribeTable("interrupts requests at their deadline", func(operation func(context.Context, *Client) error) {
		c, requests := connectedClient(func(int) []byte { return nil })
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- operation(ctx, c) }()
		awaitRequest(requests)
		Expect(awaitError(done)).To(MatchError(context.DeadlineExceeded))
		_, err := c.QueryBlockStats(context.Background())
		Expect(err).To(HaveOccurred(), "timed-out connection must not be reused")
	},
		Entry("blockstats", func(ctx context.Context, c *Client) error {
			_, err := c.QueryBlockStats(ctx)
			return err
		}),
		Entry("histogram", func(ctx context.Context, c *Client) error {
			return c.EnableHistogram(ctx, "disk", []int64{1000})
		}),
		Entry("virtio", func(ctx context.Context, c *Client) error {
			_, err := c.QueryVirtio(ctx)
			return err
		}),
		Entry("virtio-status", func(ctx context.Context, c *Client) error {
			_, err := c.QueryVirtioStatus(ctx, "disk")
			return err
		}),
		Entry("virtqueue", func(ctx context.Context, c *Client) error {
			_, err := c.QueryVirtioQueueStatus(ctx, "disk", 0)
			return err
		}),
		Entry("guest-agent", func(ctx context.Context, c *Client) error {
			_, err := c.AgentCommand(ctx, `{"execute":"guest-info"}`, 5)
			return err
		}),
	)

	It("interrupts a request when its context is canceled without a deadline", func() {
		c, requests := connectedClient(func(int) []byte { return nil })
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := c.QueryBlockStats(ctx)
			done <- err
		}()
		awaitRequest(requests)
		cancel()
		Expect(awaitError(done)).To(MatchError(context.Canceled))
	})

	It("interrupts an in-flight request when closed and allows repeated close", func() {
		c, requests := connectedClient(func(int) []byte { return nil })
		done := make(chan error, 1)
		go func() {
			_, err := c.QueryBlockStats(context.Background())
			done <- err
		}()
		awaitRequest(requests)
		closed := make(chan error, 1)
		go func() { closed <- c.Close() }()
		Expect(awaitError(closed)).NotTo(HaveOccurred())
		Expect(awaitError(done)).To(HaveOccurred())
		Expect(c.Close()).To(Succeed())
	})

	DescribeTable("interrupts connection setup at its deadline", func(stage int) {
		conn, requests := mockLibvirt(func(i int) []byte {
			if i == stage {
				return nil
			}
			return setupReply(i)
		})
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		done := make(chan error, 1)
		go func() {
			c, err := newClient(ctx, conn, "test-vm")
			if c != nil {
				c.Close()
			}
			done <- err
		}()
		for i := 0; i < stage; i++ {
			awaitRequest(requests)
		}
		Expect(awaitError(done)).To(MatchError(context.DeadlineExceeded))
	},
		Entry("authentication", 1),
		Entry("session", 2),
		Entry("domain lookup", 3),
	)

	It("keeps a successful connection usable after the request context is canceled", func() {
		c, _ := connectedClient(func(int) []byte { return xdrString(`{"return":[]}`) })
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, err := c.QueryBlockStats(ctx)
		Expect(err).NotTo(HaveOccurred())
		cancel() // Must not close the connection after the completed request.
		_, err = c.QueryBlockStats(ctx)
		Expect(err).To(MatchError(context.Canceled))
		nextCtx, nextCancel := context.WithTimeout(context.Background(), time.Second)
		defer nextCancel()
		_, err = c.QueryBlockStats(nextCtx)
		Expect(err).NotTo(HaveOccurred(), "connection must remain reusable")
	})
})
