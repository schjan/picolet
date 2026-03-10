package mqtt

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
	"github.com/eclipse/paho.golang/paho"

	"github.com/schjan/picolet/pkg/metrics"
)

// Status describes the agent state published to MQTT topics.
type Status struct {
	LastReconciliation           time.Time
	LastSuccessfulReconciliation time.Time
	AppliedSHA                   string
	FailedCount                  int
	Paused                       bool
}

// Config holds MQTT connection settings. Password is already resolved (read from file at wire-up).
type Config struct {
	BrokerURL   string // tcp://host:1883
	Username    string
	Password    string
	TopicPrefix string // default: "picolet"
}

// Client manages a long-lived MQTT connection with auto-reconnect.
type Client struct {
	cfg      Config
	hostname string
	conn     *autopaho.ConnectionManager

	// Precomputed topic strings (immutable after construction).
	pauseTopic   string
	triggerTopic string
	stateTopic   string
	statusPrefix string // base for all status subtopics

	lastStatus atomic.Pointer[Status] // last attempted status, replayed on reconnect
}

// NewClient creates a new MQTT Client. Sets TopicPrefix default to "picolet" if empty.
// Returns an error if hostname contains MQTT-reserved characters (+, #, /).
func NewClient(cfg Config, hostname string) (*Client, error) {
	if strings.ContainsAny(hostname, "#+/") {
		return nil, fmt.Errorf("hostname %q contains MQTT-reserved characters", hostname)
	}
	if cfg.TopicPrefix == "" {
		cfg.TopicPrefix = "picolet"
	}
	return &Client{
		cfg:          cfg,
		hostname:     hostname,
		pauseTopic:   cfg.TopicPrefix + "/" + hostname + "/pause",
		triggerTopic: cfg.TopicPrefix + "/trigger",
		stateTopic:   cfg.TopicPrefix + "/" + hostname + "/status/state",
		statusPrefix: cfg.TopicPrefix + "/" + hostname + "/status",
	}, nil
}

// Start connects to the MQTT broker and subscribes to pause and trigger topics.
// pauseFlag is set to true/false by the pause topic; triggerFn is called on trigger messages.
//
//nolint:funlen,cyclop // MQTT setup is inherently verbose; autopaho config is a value struct
func (c *Client) Start(ctx context.Context, pauseFlag *atomic.Bool, triggerFn func()) error {
	brokerURL, err := url.Parse(c.cfg.BrokerURL)
	if err != nil {
		return fmt.Errorf("parsing broker URL: %w", err)
	}

	cliCfg := autopaho.ClientConfig{
		ServerUrls: []*url.URL{brokerURL},
		KeepAlive:  30,
		// Start fresh on initial connect; retained messages still delivered by broker.
		CleanStartOnInitialConnection: true,
		// Session survives short WiFi drops (5 minutes).
		SessionExpiryInterval: 300,

		ConnectUsername: c.cfg.Username,
		ConnectPassword: []byte(c.cfg.Password),

		WillMessage: &paho.WillMessage{
			Topic:   c.stateTopic,
			Payload: []byte("offline"),
			QoS:     1,
			Retain:  true,
		},

		OnConnectionUp: func(cm *autopaho.ConnectionManager, _ *paho.Connack) {
			slog.Info("mqtt connected", "broker", c.cfg.BrokerURL)
			metrics.MQTTConnected.Set(1)

			// Re-subscribe on every (re)connect.
			if _, subErr := cm.Subscribe(ctx, &paho.Subscribe{
				Subscriptions: []paho.SubscribeOptions{
					{Topic: c.pauseTopic, QoS: 1},
					{Topic: c.triggerTopic, QoS: 1},
				},
			}); subErr != nil {
				slog.Error("mqtt subscribe failed", "error", subErr)
			}

			// Republish last known full status if available (reconnect after drop),
			// otherwise publish state only (first connect, no tick completed yet).
			if prev := c.lastStatus.Load(); prev != nil {
				status := *prev
				status.Paused = pauseFlag.Load() // always reflect live pause state on reconnect
				if pubErr := c.publishStatusTopics(ctx, status); pubErr != nil {
					slog.Warn("mqtt republish status on reconnect failed", "error", pubErr)
				}
			} else {
				initialState := "running"
				if pauseFlag.Load() {
					initialState = "paused"
				}
				if _, pubErr := cm.Publish(ctx, &paho.Publish{
					Topic: c.stateTopic, QoS: 1, Retain: true,
					Payload: []byte(initialState),
				}); pubErr != nil {
					slog.Warn("mqtt publish state failed", "state", initialState, "error", pubErr)
				}
			}
		},

		OnConnectionDown: func() bool {
			slog.Warn("mqtt connection lost, reconnecting", "broker", c.cfg.BrokerURL)
			metrics.MQTTConnected.Set(0)
			return true
		},

		OnConnectError: func(connectErr error) {
			slog.Warn("mqtt connection error", "error", connectErr)
			metrics.MQTTConnected.Set(0)
		},

		ClientConfig: paho.ClientConfig{
			ClientID: "picolet-" + c.hostname,
			OnClientError: func(clientErr error) {
				slog.Error("mqtt client error", "error", clientErr)
			},
			OnServerDisconnect: func(d *paho.Disconnect) {
				if d.Properties != nil {
					slog.Warn("mqtt server disconnect", "reason", d.Properties.ReasonString, "code", d.ReasonCode)
				} else {
					slog.Warn("mqtt server disconnect", "code", d.ReasonCode)
				}
			},
			OnPublishReceived: []func(paho.PublishReceived) (bool, error){
				func(pr paho.PublishReceived) (bool, error) {
					switch pr.Packet.Topic {
					case c.pauseTopic:
						paused := string(pr.Packet.Payload) == "true"
						pauseFlag.Store(paused)
						metrics.AgentPaused.Set(boolToFloat64(paused))
						slog.Info("mqtt pause state changed", "paused", paused)
					case c.triggerTopic:
						slog.Info("mqtt trigger received")
						triggerFn()
					}
					return true, nil
				},
			},
		},
	}

	cm, err := autopaho.NewConnection(ctx, cliCfg)
	if err != nil {
		return fmt.Errorf("creating MQTT connection: %w", err)
	}
	c.conn = cm
	return nil
}

// PublishStatus caches the status for reconnect replay and publishes it
// to retained MQTT topics.
func (c *Client) PublishStatus(ctx context.Context, status Status) error {
	// Cache unconditionally so OnConnectionUp can replay on reconnect.
	statusCopy := status
	c.lastStatus.Store(&statusCopy)

	return c.publishStatusTopics(ctx, status)
}

// publishStatusTopics publishes the status to retained MQTT topics without
// updating the cache. Used directly by OnConnectionUp to replay the cached
// status without a redundant re-store.
func (c *Client) publishStatusTopics(ctx context.Context, status Status) error {
	state := "running"
	if status.Paused {
		state = "paused"
	}

	topics := map[string]string{
		c.stateTopic:                                       state,
		c.statusPrefix + "/applied_sha":                    status.AppliedSHA,
		c.statusPrefix + "/failed_count":                   strconv.Itoa(status.FailedCount),
		c.statusPrefix + "/last_reconciliation":            formatTimestamp(status.LastReconciliation),
		c.statusPrefix + "/last_successful_reconciliation": formatTimestamp(status.LastSuccessfulReconciliation),
	}

	var errs []error
	for topic, payload := range topics {
		if _, err := c.conn.Publish(ctx, &paho.Publish{
			Topic: topic, QoS: 1, Retain: true,
			Payload: []byte(payload),
		}); err != nil {
			slog.Warn("mqtt publish failed", "topic", topic, "error", err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Close publishes state=offline and disconnects from the broker.
func (c *Client) Close(ctx context.Context) {
	if c.conn == nil {
		return
	}
	// Best-effort offline publish before clean disconnect.
	if _, err := c.conn.Publish(ctx, &paho.Publish{
		Topic: c.stateTopic, QoS: 1, Retain: true,
		Payload: []byte("offline"),
	}); err != nil {
		slog.Warn("mqtt publish state=offline failed", "error", err)
	}
	if err := c.conn.Disconnect(ctx); err != nil {
		slog.Warn("mqtt disconnect failed", "error", err)
	}
	metrics.MQTTConnected.Set(0)
	metrics.AgentPaused.Set(0)
}

// AwaitConnection blocks until the MQTT connection is established or the context is cancelled.
func (c *Client) AwaitConnection(ctx context.Context) error {
	return c.conn.AwaitConnection(ctx)
}

// Trigger publishes a single message to the trigger topic using a short-lived raw paho.Client.
// Intended for CLI use — no autopaho overhead.
func Trigger(ctx context.Context, cfg Config) error {
	brokerURL, err := url.Parse(cfg.BrokerURL)
	if err != nil {
		return fmt.Errorf("parsing broker URL: %w", err)
	}

	prefix := cfg.TopicPrefix
	if prefix == "" {
		prefix = "picolet"
	}

	var conn net.Conn
	switch brokerURL.Scheme {
	case "ssl", "tls":
		conn, err = tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", brokerURL.Host, nil)
	case "tcp", "mqtt":
		conn, err = net.DialTimeout("tcp", brokerURL.Host, 5*time.Second)
	default:
		return fmt.Errorf("unsupported MQTT broker scheme: %s", brokerURL.Scheme)
	}
	if err != nil {
		return fmt.Errorf("connecting to MQTT broker: %w", err)
	}
	defer conn.Close()

	c := paho.NewClient(paho.ClientConfig{Conn: conn})
	cp := &paho.Connect{
		ClientID:   fmt.Sprintf("picolet-trigger-%d", time.Now().UnixNano()),
		CleanStart: true,
		KeepAlive:  10,
	}
	if cfg.Username != "" {
		cp.Username = cfg.Username
		cp.UsernameFlag = true
		cp.Password = []byte(cfg.Password)
		cp.PasswordFlag = true
	}
	if _, err := c.Connect(ctx, cp); err != nil {
		return fmt.Errorf("MQTT connect: %w", err)
	}
	defer func() { _ = c.Disconnect(&paho.Disconnect{}) }()

	if _, err := c.Publish(ctx, &paho.Publish{
		Topic: prefix + "/trigger",
		QoS:   1,
	}); err != nil {
		return fmt.Errorf("MQTT publish: %w", err)
	}

	slog.Info("MQTT trigger published (best-effort, no delivery confirmation)")
	return nil
}

func formatTimestamp(t time.Time) string {
	if t.IsZero() {
		return "0"
	}
	return strconv.FormatInt(t.Unix(), 10)
}

func boolToFloat64(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
