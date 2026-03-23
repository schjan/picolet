package mqtt_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pahopkg "github.com/eclipse/paho.golang/paho"
	mqttserver "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/hooks/auth"
	"github.com/mochi-mqtt/server/v2/listeners"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/pkg/metrics"
	"github.com/schjan/picolet/pkg/mqtt"
)

// tWriter adapts testing.T to io.Writer so slog output lands in test logs.
// Uses a done flag to silently drop writes after the test completes,
// preventing panics from mochi-mqtt listener goroutines that log during shutdown.
type tWriter struct {
	t    *testing.T
	done atomic.Bool
}

func (w *tWriter) Write(p []byte) (int, error) {
	if w.done.Load() {
		return len(p), nil
	}
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	w := &tWriter{t: t}
	t.Cleanup(func() { w.done.Store(true) })
	return slog.New(slog.NewTextHandler(w, nil))
}

func init() {
	metrics.Register()
}

// retainedPayload reads a retained message directly from the broker's topic
// index, bypassing TCP subscribers to avoid the mochi-mqtt race between
// RetainMessage (write) and scanMessages (read).
func retainedPayload(srv *mqttserver.Server, topic string) string {
	pk, ok := srv.Topics.Retained.Get(topic)
	if !ok {
		return ""
	}
	return string(pk.Payload)
}

func startTestBroker(t *testing.T) (string, *mqttserver.Server) {
	t.Helper()
	server := mqttserver.New(&mqttserver.Options{
		Logger:       testLogger(t),
		InlineClient: true,
	})
	require.NoError(t, server.AddHook(new(auth.AllowHook), nil))

	tcp := listeners.NewTCP(listeners.Config{ID: "test-" + t.Name(), Address: "127.0.0.1:0"})
	require.NoError(t, server.AddListener(tcp))

	go func() { _ = server.Serve() }()
	t.Cleanup(func() { _ = server.Close() })

	return "tcp://" + tcp.Address(), server
}

func rawPublish(ctx context.Context, brokerURL, topic, payload string, retain bool) error {
	u, err := url.Parse(brokerURL)
	if err != nil {
		return fmt.Errorf("parsing broker URL: %w", err)
	}
	conn, err := net.DialTimeout("tcp", u.Host, 5*time.Second)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	c := pahopkg.NewClient(pahopkg.ClientConfig{Conn: conn})
	if _, err = c.Connect(ctx, &pahopkg.Connect{
		ClientID:   fmt.Sprintf("test-pub-%d", time.Now().UnixNano()),
		CleanStart: true,
		KeepAlive:  10,
	}); err != nil {
		return fmt.Errorf("connect: %w", err)
	}

	if _, err = c.Publish(ctx, &pahopkg.Publish{
		Topic:   topic,
		QoS:     1,
		Retain:  retain,
		Payload: []byte(payload),
	}); err != nil {
		_ = c.Disconnect(&pahopkg.Disconnect{})
		return fmt.Errorf("publish: %w", err)
	}

	_ = c.Disconnect(&pahopkg.Disconnect{})
	return nil
}

func TestPauseSubscription(t *testing.T) {
	t.Parallel()
	brokerURL, _ := startTestBroker(t)

	cfg := mqtt.Config{BrokerURL: brokerURL, TopicPrefix: "picolet"}
	client, err := mqtt.NewClient(cfg, "test-host")
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var pauseFlag atomic.Bool
	require.NoError(t, client.Start(ctx, &pauseFlag, func() {}))

	require.NoError(t, client.AwaitConnection(ctx))

	require.NoError(t, rawPublish(ctx, brokerURL, "picolet/test-host/pause", "true", true))

	require.Eventually(t, pauseFlag.Load, 3*time.Second, 50*time.Millisecond, "pause flag should become true")

	require.NoError(t, rawPublish(ctx, brokerURL, "picolet/test-host/pause", "false", true))

	require.Eventually(t, func() bool {
		return !pauseFlag.Load()
	}, 3*time.Second, 50*time.Millisecond, "pause flag should become false")
}

func TestTriggerSubscription(t *testing.T) {
	t.Parallel()
	brokerURL, _ := startTestBroker(t)

	cfg := mqtt.Config{BrokerURL: brokerURL, TopicPrefix: "picolet"}
	client, err := mqtt.NewClient(cfg, "test-host")
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var triggered atomic.Int32
	var pauseFlag atomic.Bool
	require.NoError(t, client.Start(ctx, &pauseFlag, func() { triggered.Add(1) }))

	require.NoError(t, client.AwaitConnection(ctx))

	require.NoError(t, rawPublish(ctx, brokerURL, "picolet/trigger", "", false))

	require.Eventually(t, func() bool {
		return triggered.Load() > 0
	}, 3*time.Second, 50*time.Millisecond, "trigger callback should be called")
}

func TestPublishStatus(t *testing.T) {
	t.Parallel()
	brokerURL, srv := startTestBroker(t)

	cfg := mqtt.Config{BrokerURL: brokerURL, TopicPrefix: "picolet"}
	client, err := mqtt.NewClient(cfg, "test-host")
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var pauseFlag atomic.Bool
	require.NoError(t, client.Start(ctx, &pauseFlag, func() {}))

	require.NoError(t, client.AwaitConnection(ctx))

	now := time.Now().Truncate(time.Second)
	status := mqtt.Status{
		LastReconciliation:           now,
		LastSuccessfulReconciliation: now,
		AppliedSHA:                   "abc123",
		FailedCount:                  2,
		Paused:                       false,
	}
	require.NoError(t, client.PublishStatus(ctx, status))

	require.Eventually(t, func() bool {
		return retainedPayload(srv, "picolet/test-host/status/applied_sha") == "abc123"
	}, 3*time.Second, 50*time.Millisecond, "retained status should be stored")

	assert.Equal(t, "abc123", retainedPayload(srv, "picolet/test-host/status/applied_sha"))
	assert.Equal(t, "2", retainedPayload(srv, "picolet/test-host/status/failed_count"))
	assert.Equal(t, "running", retainedPayload(srv, "picolet/test-host/status/state"))
	assert.Equal(t, strconv.FormatInt(now.Unix(), 10), retainedPayload(srv, "picolet/test-host/status/last_reconciliation"))
	assert.Equal(t, strconv.FormatInt(now.Unix(), 10), retainedPayload(srv, "picolet/test-host/status/last_successful_reconciliation"))
}

// newTCPProxy creates a bidirectional TCP proxy in front of the broker.
// When closeListener is true, the kill function also closes the listener
// (preventing reconnection) — suitable for LWT tests. When false, only
// active connections are dropped and the listener stays open for autopaho
// to reconnect through.
func newTCPProxy(t *testing.T, brokerURL string, closeListener bool) (string, func()) {
	t.Helper()
	u, err := url.Parse(brokerURL)
	require.NoError(t, err)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	if !closeListener {
		t.Cleanup(func() { _ = listener.Close() })
	}

	var mu sync.Mutex
	var conns []net.Conn
	var wg sync.WaitGroup

	go func() {
		for {
			clientConn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			brokerConn, dialErr := net.DialTimeout("tcp", u.Host, 5*time.Second)
			if dialErr != nil {
				_ = clientConn.Close()
				continue
			}
			mu.Lock()
			conns = append(conns, clientConn, brokerConn)
			mu.Unlock()
			wg.Go(func() { _, _ = io.Copy(brokerConn, clientConn) })
			wg.Go(func() { _, _ = io.Copy(clientConn, brokerConn) })
		}
	}()

	killConns := func() {
		mu.Lock()
		for _, c := range conns {
			_ = c.Close()
		}
		conns = nil
		mu.Unlock()
		// No wg.Wait() — connections are closed so io.Copy returns quickly;
		// test proceeds without blocking on goroutine drain.
	}

	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			killConns()
			if closeListener {
				_ = listener.Close()
			}
			wg.Wait() // safe at test teardown: context is cancelled, no new reconnects
		})
	}
	t.Cleanup(cleanup)

	// For closeListener=true (LWT tests): return the full cleanup so the caller's
	// kill() also closes the listener, preventing autopaho from reconnecting after
	// the simulated drop (restoring the pre-refactor behaviour).
	if closeListener {
		return listener.Addr().String(), cleanup
	}
	return listener.Addr().String(), killConns
}

func startTCPProxy(t *testing.T, brokerURL string) (string, func()) {
	t.Helper()
	return newTCPProxy(t, brokerURL, true)
}

//nolint:funlen // LWT test requires proxy setup + two clients + assertions
func TestLWT(t *testing.T) {
	t.Parallel()
	brokerURL, _ := startTestBroker(t)
	proxyAddr, killProxy := startTCPProxy(t, brokerURL)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Subscribe the observer BEFORE the picolet client connects.
	// When no retained messages exist yet, the broker's Messages() returns
	// immediately (Retained.Len()==0), avoiding the mochi-mqtt race between
	// scanMessages reads and RetainMessage writes on particle.retainPath.
	var mu sync.Mutex
	var states []string

	u, err := url.Parse(brokerURL)
	require.NoError(t, err)
	conn, err := net.DialTimeout("tcp", u.Host, 5*time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	sub := pahopkg.NewClient(pahopkg.ClientConfig{
		Conn: conn,
		OnPublishReceived: []func(pahopkg.PublishReceived) (bool, error){
			func(pr pahopkg.PublishReceived) (bool, error) {
				if pr.Packet.Topic == "picolet/lwt-host/status/state" {
					mu.Lock()
					states = append(states, string(pr.Packet.Payload))
					mu.Unlock()
				}
				return true, nil
			},
		},
	})
	_, err = sub.Connect(ctx, &pahopkg.Connect{
		ClientID:   "test-lwt-sub",
		CleanStart: true,
		KeepAlive:  30,
	})
	require.NoError(t, err)

	_, err = sub.Subscribe(ctx, &pahopkg.Subscribe{
		Subscriptions: []pahopkg.SubscribeOptions{
			{Topic: "picolet/lwt-host/status/state", QoS: 1},
		},
	})
	require.NoError(t, err)

	// Now connect the picolet client (through proxy).
	// OnConnectionUp publishes state=running (retained) — delivered to the
	// already-subscribed observer via normal publish, not via Messages().
	cfg := mqtt.Config{BrokerURL: "tcp://" + proxyAddr, TopicPrefix: "picolet"}
	client, err := mqtt.NewClient(cfg, "lwt-host")
	require.NoError(t, err)

	var pauseFlag atomic.Bool
	require.NoError(t, client.Start(ctx, &pauseFlag, func() {}))

	require.NoError(t, client.AwaitConnection(ctx))

	// Verify we receive "running" from the OnConnectionUp callback.
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return slices.Contains(states, "running")
	}, 3*time.Second, 50*time.Millisecond, "should receive state=running")

	// Kill the proxy — simulates network loss / SIGKILL.
	// The broker sees a TCP drop without DISCONNECT → fires LWT (state=offline).
	killProxy()

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		// LWT "offline" must appear after "running".
		seenRunning := false
		for _, s := range states {
			if s == "running" {
				seenRunning = true
			}
			if seenRunning && s == "offline" {
				return true
			}
		}
		return false
	}, 5*time.Second, 50*time.Millisecond, "broker should publish LWT state=offline after forced disconnect")
}

func startRestartableTCPProxy(t *testing.T, brokerURL string) (string, func()) {
	t.Helper()
	return newTCPProxy(t, brokerURL, false)
}

func TestReconnectRepublishesStatus(t *testing.T) {
	t.Parallel()
	brokerURL, srv := startTestBroker(t)
	proxyAddr, killConns := startRestartableTCPProxy(t, brokerURL)

	cfg := mqtt.Config{BrokerURL: "tcp://" + proxyAddr, TopicPrefix: "picolet", ConnectRetryDelay: 500 * time.Millisecond}
	client, err := mqtt.NewClient(cfg, "reconnect-host")
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var pauseFlag atomic.Bool
	require.NoError(t, client.Start(ctx, &pauseFlag, func() {}))
	require.NoError(t, client.AwaitConnection(ctx))

	// Publish a known status.
	now := time.Now().Truncate(time.Second)
	status := mqtt.Status{
		LastReconciliation:           now,
		LastSuccessfulReconciliation: now,
		AppliedSHA:                   "deadbeef",
		FailedCount:                  1,
		Paused:                       false,
	}
	require.NoError(t, client.PublishStatus(ctx, status))

	verifyRetainedStatus := func(label string, timeout time.Duration) {
		t.Helper()
		require.Eventually(t, func() bool {
			return retainedPayload(srv, "picolet/reconnect-host/status/applied_sha") == "deadbeef"
		}, timeout, 50*time.Millisecond, "%s: applied_sha should be deadbeef", label)

		assert.Equal(t, "deadbeef", retainedPayload(srv, "picolet/reconnect-host/status/applied_sha"), "%s: applied_sha", label)
		assert.Equal(t, "1", retainedPayload(srv, "picolet/reconnect-host/status/failed_count"), "%s: failed_count", label)
		assert.Equal(t, "running", retainedPayload(srv, "picolet/reconnect-host/status/state"), "%s: state", label)
		assert.Equal(t, strconv.FormatInt(now.Unix(), 10), retainedPayload(srv, "picolet/reconnect-host/status/last_reconciliation"), "%s: last_reconciliation", label)
		assert.Equal(t, strconv.FormatInt(now.Unix(), 10), retainedPayload(srv, "picolet/reconnect-host/status/last_successful_reconciliation"), "%s: last_successful_reconciliation", label)
	}

	verifyRetainedStatus("before-reconnect", 5*time.Second)

	// Poison one retained topic directly on the broker (inline publish, bypassing
	// proxy) to prove republish is needed. After reconnect, if applied_sha ==
	// "deadbeef" again, OnConnectionUp provably republished.
	require.NoError(t, srv.Publish("picolet/reconnect-host/status/applied_sha", []byte("poisoned"), true, 1))

	killConns()

	// Wait for the full disconnect → reconnect → republish cycle.
	// Don't rely on AwaitConnection here: autopaho may not have detected the
	// TCP drop yet, so AwaitConnection can return nil (still "connected")
	// before any reconnect actually occurs. Instead, poll the retained topic
	// directly — that's the actual invariant under test.
	verifyRetainedStatus("after-reconnect", 10*time.Second)
}

func TestTriggerFunction(t *testing.T) {
	t.Parallel()
	brokerURL, _ := startTestBroker(t)

	cfg := mqtt.Config{BrokerURL: brokerURL, TopicPrefix: "picolet"}
	subClient, err := mqtt.NewClient(cfg, "trigger-test-host")
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var triggered atomic.Int32
	var pauseFlag atomic.Bool
	require.NoError(t, subClient.Start(ctx, &pauseFlag, func() { triggered.Add(1) }))

	require.NoError(t, subClient.AwaitConnection(ctx))

	triggerCtx, triggerCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer triggerCancel()
	require.NoError(t, mqtt.Trigger(triggerCtx, cfg))

	require.Eventually(t, func() bool {
		return triggered.Load() > 0
	}, 3*time.Second, 50*time.Millisecond, "trigger message should be received")
}
