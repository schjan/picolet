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
type tWriter struct{ t *testing.T }

func (w tWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(tWriter{t}, nil))
}

func init() {
	metrics.Register()
}

func startTestBroker(t *testing.T) string {
	t.Helper()
	server := mqttserver.New(&mqttserver.Options{Logger: testLogger(t)})
	require.NoError(t, server.AddHook(new(auth.AllowHook), nil))

	tcp := listeners.NewTCP(listeners.Config{ID: "test-" + t.Name(), Address: "127.0.0.1:0"})
	require.NoError(t, server.AddListener(tcp))

	go func() { _ = server.Serve() }()
	t.Cleanup(func() { _ = server.Close() })

	return "tcp://" + tcp.Address()
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
	brokerURL := startTestBroker(t)

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
	brokerURL := startTestBroker(t)

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

//nolint:funlen // setup-heavy integration test: publisher + subscriber + assertions
func TestPublishStatus(t *testing.T) {
	t.Parallel()
	brokerURL := startTestBroker(t)

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

	// Subscribe from a second client and collect retained messages.
	var mu sync.Mutex
	received := make(map[string]string)

	// Use a second subscriber to capture retained messages.
	u, err := url.Parse(brokerURL)
	require.NoError(t, err)
	conn, err := net.DialTimeout("tcp", u.Host, 5*time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	sub := pahopkg.NewClient(pahopkg.ClientConfig{
		Conn: conn,
		OnPublishReceived: []func(pahopkg.PublishReceived) (bool, error){
			func(pr pahopkg.PublishReceived) (bool, error) {
				mu.Lock()
				received[pr.Packet.Topic] = string(pr.Packet.Payload)
				mu.Unlock()
				return true, nil
			},
		},
	})
	_, err = sub.Connect(ctx, &pahopkg.Connect{
		ClientID:   "test-sub",
		CleanStart: true,
		KeepAlive:  10,
	})
	require.NoError(t, err)

	_, err = sub.Subscribe(ctx, &pahopkg.Subscribe{
		Subscriptions: []pahopkg.SubscribeOptions{
			{Topic: "picolet/test-host/status/#", QoS: 1},
		},
	})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(received) >= 5
	}, 3*time.Second, 50*time.Millisecond, "all 5 status topics should be published")

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, "abc123", received["picolet/test-host/status/applied_sha"])
	assert.Equal(t, "2", received["picolet/test-host/status/failed_count"])
	assert.Equal(t, "running", received["picolet/test-host/status/state"])
	assert.Equal(t, strconv.FormatInt(now.Unix(), 10), received["picolet/test-host/status/last_reconciliation"])
	assert.Equal(t, strconv.FormatInt(now.Unix(), 10), received["picolet/test-host/status/last_successful_reconciliation"])
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

	cleanup := func() {
		killConns()
		if closeListener {
			_ = listener.Close()
		}
		wg.Wait() // safe at test teardown: context is cancelled, no new reconnects
	}
	t.Cleanup(cleanup)

	return listener.Addr().String(), killConns
}

// poisonRetained overwrites a retained MQTT topic directly on the broker (bypassing any proxy).
// Used to ensure the client must actively republish to restore correct retained values.
func poisonRetained(ctx context.Context, t *testing.T, brokerURL, topic, payload string) {
	t.Helper()
	u, err := url.Parse(brokerURL)
	require.NoError(t, err)
	conn, err := net.DialTimeout("tcp", u.Host, 5*time.Second)
	require.NoError(t, err)
	defer conn.Close()
	c := pahopkg.NewClient(pahopkg.ClientConfig{Conn: conn})
	_, err = c.Connect(ctx, &pahopkg.Connect{
		ClientID:   fmt.Sprintf("poison-%d", time.Now().UnixNano()),
		CleanStart: true,
		KeepAlive:  10,
	})
	require.NoError(t, err)
	_, err = c.Publish(ctx, &pahopkg.Publish{
		Topic: topic, QoS: 1, Retain: true, Payload: []byte(payload),
	})
	require.NoError(t, err)
}

func startTCPProxy(t *testing.T, brokerURL string) (string, func()) {
	t.Helper()
	return newTCPProxy(t, brokerURL, true)
}

//nolint:funlen // LWT test requires proxy setup + two clients + assertions
func TestLWT(t *testing.T) {
	t.Parallel()
	brokerURL := startTestBroker(t)
	proxyAddr, killProxy := startTCPProxy(t, brokerURL)

	cfg := mqtt.Config{BrokerURL: "tcp://" + proxyAddr, TopicPrefix: "picolet"}
	client, err := mqtt.NewClient(cfg, "lwt-host")
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var pauseFlag atomic.Bool
	require.NoError(t, client.Start(ctx, &pauseFlag, func() {}))

	require.NoError(t, client.AwaitConnection(ctx))

	// Subscribe directly to the broker (not through proxy) to watch state topic.
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

//nolint:funlen // setup-heavy reconnect test: proxy + publisher + subscriber + assertions
func TestReconnectRepublishesStatus(t *testing.T) {
	t.Parallel()
	brokerURL := startTestBroker(t)
	proxyAddr, killConns := startRestartableTCPProxy(t, brokerURL)

	cfg := mqtt.Config{BrokerURL: "tcp://" + proxyAddr, TopicPrefix: "picolet"}
	client, err := mqtt.NewClient(cfg, "reconnect-host")
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
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

	// Verify initial publish landed by subscribing directly to the broker.
	verifyRetainedStatus := func(label string) {
		t.Helper()
		var mu sync.Mutex
		received := make(map[string]string)

		u, parseErr := url.Parse(brokerURL)
		require.NoError(t, parseErr)
		conn, dialErr := net.DialTimeout("tcp", u.Host, 5*time.Second)
		require.NoError(t, dialErr)
		defer conn.Close()

		sub := pahopkg.NewClient(pahopkg.ClientConfig{
			Conn: conn,
			OnPublishReceived: []func(pahopkg.PublishReceived) (bool, error){
				func(pr pahopkg.PublishReceived) (bool, error) {
					mu.Lock()
					received[pr.Packet.Topic] = string(pr.Packet.Payload)
					mu.Unlock()
					return true, nil
				},
			},
		})
		_, connErr := sub.Connect(ctx, &pahopkg.Connect{
			ClientID:   fmt.Sprintf("test-verify-%s-%d", label, time.Now().UnixNano()),
			CleanStart: true,
			KeepAlive:  10,
		})
		require.NoError(t, connErr)

		_, subErr := sub.Subscribe(ctx, &pahopkg.Subscribe{
			Subscriptions: []pahopkg.SubscribeOptions{
				{Topic: "picolet/reconnect-host/status/#", QoS: 1},
			},
		})
		require.NoError(t, subErr)

		require.Eventually(t, func() bool {
			mu.Lock()
			defer mu.Unlock()
			// Poll until all 5 topics are present AND applied_sha has the expected value.
			// This handles the race where the subscriber connects while "poisoned" is the
			// retained value: we keep polling until the republish overwrites it.
			return len(received) >= 5 &&
				received["picolet/reconnect-host/status/applied_sha"] == "deadbeef"
		}, 5*time.Second, 50*time.Millisecond, "%s: all 5 status topics with correct values", label)

		mu.Lock()
		defer mu.Unlock()
		assert.Equal(t, "deadbeef", received["picolet/reconnect-host/status/applied_sha"], "%s: applied_sha", label)
		assert.Equal(t, "1", received["picolet/reconnect-host/status/failed_count"], "%s: failed_count", label)
		assert.Equal(t, "running", received["picolet/reconnect-host/status/state"], "%s: state", label)
		assert.Equal(t, strconv.FormatInt(now.Unix(), 10), received["picolet/reconnect-host/status/last_reconciliation"], "%s: last_reconciliation", label)
		assert.Equal(t, strconv.FormatInt(now.Unix(), 10), received["picolet/reconnect-host/status/last_successful_reconciliation"], "%s: last_successful_reconciliation", label)
	}

	verifyRetainedStatus("before-reconnect")

	// Poison one retained topic directly on the broker (bypassing proxy) to prove
	// republish is needed. After reconnect, if applied_sha == "deadbeef" again,
	// OnConnectionUp provably republished — the broker's retained "poisoned" was overwritten.
	// poisonRetained waits for PUBACK (QoS 1), so the broker has stored it before killConns.
	poisonRetained(ctx, t, brokerURL, "picolet/reconnect-host/status/applied_sha", "poisoned")

	killConns()

	require.Eventually(t, func() bool {
		return client.AwaitConnection(ctx) == nil
	}, 10*time.Second, 100*time.Millisecond, "client should reconnect after kill")

	// No time.Sleep needed — verifyRetainedStatus now polls for the correct value,
	// handling the race where the subscriber briefly sees the "poisoned" retained message
	// before the republish arrives.
	verifyRetainedStatus("after-reconnect")
}

func TestTriggerFunction(t *testing.T) {
	t.Parallel()
	brokerURL := startTestBroker(t)

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
