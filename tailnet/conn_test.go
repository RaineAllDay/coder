package tailnet_test

import (
	"context"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"cdr.dev/slog"
	"cdr.dev/slog/sloggers/sloghuman"
	"cdr.dev/slog/sloggers/slogtest"
	"github.com/coder/coder/tailnet"
	"github.com/coder/coder/tailnet/tailnettest"
	"github.com/coder/coder/testutil"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func TestTailnet(t *testing.T) {
	t.Parallel()
	logger := slogtest.Make(t, nil).Leveled(slog.LevelDebug)
	derpMap := tailnettest.RunDERPAndSTUN(t)
	t.Run("InstantClose", func(t *testing.T) {
		t.Parallel()
		conn, err := tailnet.NewConn(&tailnet.Options{
			Addresses: []netip.Prefix{netip.PrefixFrom(tailnet.IP(), 128)},
			Logger:    logger.Named("w1"),
			DERPMap:   derpMap,
		})
		require.NoError(t, err)
		err = conn.Close()
		require.NoError(t, err)
	})
	t.Run("Connect", func(t *testing.T) {
		t.Parallel()
		w1IP := tailnet.IP()
		w1, err := tailnet.NewConn(&tailnet.Options{
			Addresses: []netip.Prefix{netip.PrefixFrom(w1IP, 128)},
			Logger:    logger.Named("w1"),
			DERPMap:   derpMap,
		})
		require.NoError(t, err)

		w2, err := tailnet.NewConn(&tailnet.Options{
			Addresses: []netip.Prefix{netip.PrefixFrom(tailnet.IP(), 128)},
			Logger:    logger.Named("w2"),
			DERPMap:   derpMap,
		})
		require.NoError(t, err)
		t.Cleanup(func() {
			_ = w1.Close()
			_ = w2.Close()
		})
		w1.SetNodeCallback(func(node *tailnet.Node) {
			err := w2.UpdateNodes([]*tailnet.Node{node}, false)
			assert.NoError(t, err)
		})
		w2.SetNodeCallback(func(node *tailnet.Node) {
			err := w1.UpdateNodes([]*tailnet.Node{node}, false)
			assert.NoError(t, err)
		})
		require.True(t, w2.AwaitReachable(context.Background(), w1IP))
		conn := make(chan struct{}, 1)
		go func() {
			listener, err := w1.Listen("tcp", ":35565")
			assert.NoError(t, err)
			defer listener.Close()
			nc, err := listener.Accept()
			if !assert.NoError(t, err) {
				return
			}
			_ = nc.Close()
			conn <- struct{}{}
		}()

		nc, err := w2.DialContextTCP(context.Background(), netip.AddrPortFrom(w1IP, 35565))
		require.NoError(t, err)
		_ = nc.Close()
		<-conn

		nodes := make(chan *tailnet.Node, 1)
		w2.SetNodeCallback(func(node *tailnet.Node) {
			select {
			case nodes <- node:
			default:
			}
		})
		node := <-nodes
		// Ensure this connected over DERP!
		require.Len(t, node.DERPForcedWebsocket, 0)

		w1.Close()
		w2.Close()
	})

	t.Run("ForcesWebSockets", func(t *testing.T) {
		t.Parallel()
		ctx := testutil.Context(t, testutil.WaitLong)

		w1IP := tailnet.IP()
		derpMap := tailnettest.RunDERPOnlyWebSockets(t)
		w1, err := tailnet.NewConn(&tailnet.Options{
			Addresses:      []netip.Prefix{netip.PrefixFrom(w1IP, 128)},
			Logger:         logger.Named("w1"),
			DERPMap:        derpMap,
			BlockEndpoints: true,
		})
		require.NoError(t, err)

		w2, err := tailnet.NewConn(&tailnet.Options{
			Addresses:      []netip.Prefix{netip.PrefixFrom(tailnet.IP(), 128)},
			Logger:         logger.Named("w2"),
			DERPMap:        derpMap,
			BlockEndpoints: true,
		})
		require.NoError(t, err)
		t.Cleanup(func() {
			_ = w1.Close()
			_ = w2.Close()
		})
		w1.SetNodeCallback(func(node *tailnet.Node) {
			err := w2.UpdateNodes([]*tailnet.Node{node}, false)
			assert.NoError(t, err)
		})
		w2.SetNodeCallback(func(node *tailnet.Node) {
			err := w1.UpdateNodes([]*tailnet.Node{node}, false)
			assert.NoError(t, err)
		})
		require.True(t, w2.AwaitReachable(ctx, w1IP))
		conn := make(chan struct{}, 1)
		go func() {
			listener, err := w1.Listen("tcp", ":35565")
			assert.NoError(t, err)
			defer listener.Close()
			nc, err := listener.Accept()
			if !assert.NoError(t, err) {
				return
			}
			_ = nc.Close()
			conn <- struct{}{}
		}()

		nc, err := w2.DialContextTCP(ctx, netip.AddrPortFrom(w1IP, 35565))
		require.NoError(t, err)
		_ = nc.Close()
		<-conn

		nodes := make(chan *tailnet.Node, 1)
		w2.SetNodeCallback(func(node *tailnet.Node) {
			select {
			case nodes <- node:
			default:
			}
		})
		node := <-nodes
		require.Len(t, node.DERPForcedWebsocket, 1)
		// Ensure the reason is valid!
		require.Equal(t, `GET failed with status code 400 (a proxy could be disallowing the use of 'Upgrade: derp'): Invalid "Upgrade" header: DERP`, node.DERPForcedWebsocket[derpMap.RegionIDs()[0]])

		w1.Close()
		w2.Close()
	})
}

// TestConn_PreferredDERP tests that we only trigger the NodeCallback when we have a preferred DERP server.
func TestConn_PreferredDERP(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testutil.WaitShort)
	defer cancel()
	logger := slogtest.Make(t, nil).Leveled(slog.LevelDebug)
	derpMap := tailnettest.RunDERPAndSTUN(t)
	conn, err := tailnet.NewConn(&tailnet.Options{
		Addresses: []netip.Prefix{netip.PrefixFrom(tailnet.IP(), 128)},
		Logger:    logger.Named("w1"),
		DERPMap:   derpMap,
	})
	require.NoError(t, err)
	defer func() {
		err := conn.Close()
		require.NoError(t, err)
	}()
	// buffer channel so callback doesn't block
	nodes := make(chan *tailnet.Node, 50)
	conn.SetNodeCallback(func(node *tailnet.Node) {
		nodes <- node
	})
	select {
	case node := <-nodes:
		require.Equal(t, 1, node.PreferredDERP)
	case <-ctx.Done():
		t.Fatal("timed out waiting for node")
	}
}

func TestTransmitHang(t *testing.T) {
	t.Parallel()

	// Not using t.TempDir() here so that we keep logs afterwards.
	captureDir, err := os.MkdirTemp("", "tailnet-test-")
	require.NoError(t, err)

	testLog, err := os.Create(filepath.Join(captureDir, "test.log"))
	require.NoError(t, err)
	defer testLog.Close()
	recvCapture, err := os.Create(filepath.Join(captureDir, "recv.pcap"))
	require.NoError(t, err)
	defer recvCapture.Close()
	sendCapture, err := os.Create(filepath.Join(captureDir, "send.pcap"))
	require.NoError(t, err)
	defer sendCapture.Close()

	logger := slogtest.Make(t, nil).
		Leveled(slog.LevelDebug).
		AppendSinks(sloghuman.Sink(testLog))

	t.Logf("test log file: %v", testLog.Name())
	t.Logf("recv capture file: %v", recvCapture.Name())
	t.Logf("send capture file: %v", sendCapture.Name())

	derpMap := tailnettest.RunDERPAndSTUN(t)
	updateNodes := func(c *tailnet.Conn) func(*tailnet.Node) {
		return func(node *tailnet.Node) {
			err := c.UpdateNodes([]*tailnet.Node{node}, false)
			assert.NoError(t, err)
		}
	}

	recvIP := tailnet.IP()
	recv, err := tailnet.NewConn(&tailnet.Options{
		Addresses: []netip.Prefix{netip.PrefixFrom(recvIP, 128)},
		Logger:    logger.Named("recv"),
		DERPMap:   derpMap,
	})
	require.NoError(t, err)
	defer recv.Close()
	recvCaptureStop := recv.Capture(recvCapture)
	defer recvCaptureStop()

	send, err := tailnet.NewConn(&tailnet.Options{
		Addresses: []netip.Prefix{netip.PrefixFrom(tailnet.IP(), 128)},
		Logger:    logger.Named("send"),
		DERPMap:   derpMap,
	})
	require.NoError(t, err)
	defer send.Close()
	sendCaptureStop := send.Capture(sendCapture)
	defer sendCaptureStop()

	recv.SetNodeCallback(updateNodes(send))
	send.SetNodeCallback(updateNodes(recv))

	ctx, cancel := context.WithTimeout(context.Background(), testutil.WaitLong)
	defer cancel()

	require.True(t, send.AwaitReachable(ctx, recvIP))

	copyDone := make(chan struct{})
	go func() {
		defer close(copyDone)

		ln, err := recv.Listen("tcp", ":35565")
		if !assert.NoError(t, err) {
			return
		}
		defer ln.Close()

		r, err := ln.Accept()
		if !assert.NoError(t, err) {
			return
		}
		defer r.Close()

		_, err = io.Copy(io.Discard, r)
		assert.NoError(t, err)
	}()

	w, err := send.DialContextTCP(ctx, netip.AddrPortFrom(recvIP, 35565))
	require.NoError(t, err)

	now := time.Now()

	payload := []byte(strings.Repeat("hello world\n", 65536/12))
	size := 0
	for i := 0; i < 1024*2; i++ {
		logger.Debug(ctx, "write payload", slog.F("num", i), slog.F("transmitted_kb", size/1024))
		n, err := w.Write(payload)
		require.NoError(t, err)
		size += n
	}

	err = w.Close()
	require.NoError(t, err)

	<-copyDone

	if time.Since(now) > 10*time.Second {
		t.Fatal("took too long to transmit")
	}
}
