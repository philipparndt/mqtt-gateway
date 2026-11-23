package mqtt

import (
	"context"
	"encoding/json"
	"expvar"
	"math/rand"
	"os"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	PAHO "github.com/eclipse/paho.mqtt.golang"
	"github.com/philipparndt/go-logger"
	"github.com/philipparndt/mqtt-gateway/config"
)

var messagesPublishedCtr int

var (
	LogPayloadTruncate      = true
	LogPayloadTruncateBytes = 100

	// LogMessagesPublishedLogger is the logger function used for the hourly
	// "Messages published (last hour)" summary line. Defaults to logger.Debug
	// so the noise stays out of normal `info`-level operation; callers that
	// want it visible can set it to logger.Info, logger.Warn, etc. before
	// calling Start.
	LogMessagesPublishedLogger = logger.Debug

	// RetryInterval is the delay between connection attempts while
	// StartContext waits for the broker.
	RetryInterval = 5 * time.Second

	// MaxReconnectInterval caps the backoff between reconnect attempts after
	// a lost connection. Zero keeps paho's default (10 minutes). Set it
	// before calling Start or StartContext.
	MaxReconnectInterval time.Duration

	// DropWhenDisconnected makes the publish functions drop (and debug-log)
	// messages while the broker is not connected, instead of handing them to
	// paho, where the caller waits until the connection is back. Use
	// SetOnReconnect to republish state that was dropped during an outage.
	DropWhenDisconnected = false

	// PublishTimeout bounds how long a publish waits for its completion.
	// Zero waits without limit.
	PublishTimeout time.Duration
)

const (
	statusOnline  = "online"
	statusOffline = "offline"
)

var client PAHO.Client
var cfg config.MQTTConfig

var (
	onReconnect   func()
	onReconnectMu sync.Mutex
)

// SetOnReconnect registers a callback that runs every time the connection is
// re-established after it was lost, once the status topic and subscriptions
// are restored. It does not run for the first connection.
func SetOnReconnect(f func()) {
	onReconnectMu.Lock()
	defer onReconnectMu.Unlock()
	onReconnect = f
}

type OnMessageListener func(string, []byte)

type subscription struct {
	topic     string
	callback  PAHO.MessageHandler
}

var (
	subscriptions   []subscription
	subscriptionsMu sync.Mutex
)

// Start connects to the broker and returns once connected. It exits the
// process when the first connection attempt fails; use StartContext to wait
// for the broker instead.
func Start(config config.MQTTConfig, clientIdPrefix string) {
	newClient(config, clientIdPrefix)
	if token := client.Connect(); token.Wait() && token.Error() != nil {
		logger.Error("Error connecting to MQTT broker", "error", token.Error())
		os.Exit(1)
	}
	connected()
}

// StartContext connects to the broker and returns once connected. Unlike
// Start it retries every RetryInterval while the broker is not reachable. It
// returns ctx.Err() when ctx is cancelled before a connection could be made.
func StartContext(ctx context.Context, config config.MQTTConfig, clientIdPrefix string) error {
	newClient(config, clientIdPrefix)
	for {
		token := client.Connect()
		select {
		case <-token.Done():
		case <-ctx.Done():
			client.Disconnect(0)
			return ctx.Err()
		}
		if token.Error() == nil {
			connected()
			return nil
		}
		logger.Error("MQTT broker not available, retrying",
			"url", config.URL, "error", token.Error(), "retryIn", RetryInterval)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(RetryInterval):
		}
	}
}

// Stop ends the MQTT session cleanly, waiting up to 250 ms for in-flight work.
// Safe to call when never started or already disconnected.
//
// This matters for the retained status topic. The client registers "offline" as
// its will, and the broker delivers a will when a connection drops
// *unexpectedly*. A clean DISCONNECT discards it instead — which is exactly what
// a planned shutdown wants: a process that is being replaced must not announce
// its own death, because its successor has usually already published "online"
// and the late "offline" would overwrite it.
//
// Until now that was not possible. connect() blocks in select{} forever, so the
// deferred Disconnect it held could never run, and every exit tore the TCP
// connection down instead of ending the session — firing the will each time.
// Under a rolling deployment that leaves the status topic reading "offline"
// while the service is up.
//
// Callers should invoke Stop from their signal handler before main returns.
func Stop() {
	if client == nil || !client.IsConnected() {
		return
	}

	logger.Info("Disconnecting from MQTT broker")
	client.Disconnect(250)
}

func generateRandomClientID(length int) string {
	charset := "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	seededRand := rand.New(rand.NewSource(time.Now().UnixNano()))
	result := make([]byte, length)
	for i := range result {
		result[i] = charset[seededRand.Intn(len(charset))]
	}
	return string(result)
}

func LogMessagesPublished() {
	for {
		time.Sleep(time.Hour)
		LogMessagesPublishedLogger("Messages published (last hour)", "count", messagesPublishedCtr)
		messagesPublishedCtr = 0
	}
}

var (
	statsMu           sync.Mutex
	mqttConnectedAt   time.Time
	mqttReconnects    int
	mqttLastReconnect time.Time
)

func init() {
	expvar.Publish("mqtt", expvar.Func(func() any {
		subscriptionsMu.Lock()
		topics := make([]string, len(subscriptions))
		for i, s := range subscriptions {
			topics[i] = s.topic
		}
		subscriptionsMu.Unlock()

		statsMu.Lock()
		defer statsMu.Unlock()
		info := map[string]any{
			"connected_at":    mqttConnectedAt.Format(time.RFC3339),
			"reconnects":      mqttReconnects,
			"subscriptions":   topics,
		}
		if !mqttLastReconnect.IsZero() {
			info["last_reconnect"] = mqttLastReconnect.Format(time.RFC3339)
		}
		return info
	}))
}

func resubscribe() {
	subscriptionsMu.Lock()
	subs := make([]subscription, len(subscriptions))
	copy(subs, subscriptions)
	subscriptionsMu.Unlock()

	for _, s := range subs {
		logger.Info("Re-subscribing to topic", "topic", s.topic)
		client.Subscribe(s.topic, cfg.QoS, s.callback)
	}
}

// isStaleOffline reports whether a payload seen on our own status topic
// contradicts the fact that we are connected and have to say so again.
func isStaleOffline(payload []byte) bool {
	return string(payload) == statusOffline
}

// guardStatus keeps `<topic>/bridge/state` truthful. The will is the only
// thing that ever writes "offline", and it belongs to a *connection*, not to
// the service: during a rolling update the new pod announces "online" and then
// the old pod dies and its retained will lands on top, leaving a running
// service marked offline until its next restart. Watching our own status topic
// and re-asserting "online" fixes that ordering whichever way it happens, and
// costs nothing while the state is correct.
func guardStatus(statusTopic string) {
	client.Subscribe(statusTopic, 1, func(_ PAHO.Client, message PAHO.Message) {
		if !isStaleOffline(message.Payload()) {
			return
		}
		// Off the callback goroutine: waiting for a publish token inside a
		// paho message handler blocks the ack it is waiting for.
		go func() {
			logger.Info("Status topic says offline while we are connected, correcting", "topic", statusTopic)
			PublishAbsolute(statusTopic, statusOnline, true)
		}()
	})
}

// newClient builds the paho client for config and stores it in client.
func newClient(config config.MQTTConfig, clientIdPrefix string) {
	cfg = config
	statusTopic := cfg.Topic + "/bridge/state"
	clientID := clientIdPrefix + "_" + generateRandomClientID(10)
	logger.Debug("Generated client ID", "clientID", clientID)

	var reconnect atomic.Bool

	opts := PAHO.NewClientOptions().
		AddBroker(config.URL).
		SetClientID(clientID).
		SetWill(statusTopic, statusOffline, 1, true)

	opts.Password = config.Password
	opts.Username = config.Username
	if MaxReconnectInterval > 0 {
		opts.SetMaxReconnectInterval(MaxReconnectInterval)
	}

	opts.SetOnConnectHandler(func(_ PAHO.Client) {
		if !reconnect.Swap(true) {
			statsMu.Lock()
			mqttConnectedAt = time.Now()
			statsMu.Unlock()
			return
		}

		statsMu.Lock()
		mqttReconnects++
		mqttLastReconnect = time.Now()
		reconnects := mqttReconnects
		statsMu.Unlock()
		logger.Info("Reconnected to MQTT broker, re-subscribing", "reconnects", reconnects)

		PublishAbsolute(statusTopic, statusOnline, true)
		guardStatus(statusTopic)
		resubscribe()

		onReconnectMu.Lock()
		cb := onReconnect
		onReconnectMu.Unlock()
		if cb != nil {
			cb()
		}
	})

	opts.SetConnectionLostHandler(func(_ PAHO.Client, err error) {
		logger.Error("MQTT connection lost", "error", err)
	})

	client = PAHO.NewClient(opts)
}

// startOnce guards the work that must happen only for the first successful
// start of the process.
var startOnce sync.Once

// connected announces the service after the first successful connection.
func connected() {
	statusTopic := cfg.Topic + "/bridge/state"

	// Always retained, whatever cfg.Retain says: the will is retained, so a
	// non-retained "online" left every late subscriber reading "offline".
	PublishAbsolute(statusTopic, statusOnline, true)
	guardStatus(statusTopic)

	logger.Info("Connected to MQTT broker", "url", cfg.URL)
	startOnce.Do(func() { go LogMessagesPublished() })
}

func logMessage(message any) any {
	if b, ok := message.([]byte); ok && utf8.Valid(b) {
		return string(b)
	}
	return message
}

func payloadSize(message any) (int, bool) {
	switch v := message.(type) {
	case []byte:
		return len(v), true
	case string:
		return len(v), true
	default:
		return 0, false
	}
}

func PublishAbsolute(topic string, message any, retained bool) {
	if DropWhenDisconnected && (client == nil || !client.IsConnectionOpen()) {
		logger.Debug("MQTT not connected, dropping message", "topic", topic)
		return
	}

	token := client.Publish(topic, cfg.QoS, retained, message)
	if PublishTimeout > 0 {
		if !token.WaitTimeout(PublishTimeout) {
			logger.Error("Timeout publishing message", "topic", topic)
			return
		}
	} else {
		token.Wait()
	}

	messagesPublishedCtr++
	if size, ok := payloadSize(message); LogPayloadTruncate && ok && size > LogPayloadTruncateBytes {
		logger.Debug("Published message", "topic", topic, "bytes", size)
	} else {
		logger.Debug("Published message", "topic", topic, "message", logMessage(message))
	}

	if token.Error() != nil {
		logger.Error("Error publishing message", "error", token.Error())
	}
}

func PublishJSON(topic string, data any) {
	jsonData, err := json.Marshal(data)
	if err != nil {
		logger.Error("Error marshaling to JSON", "error", err)
	} else {
		PublishAbsolute(cfg.Topic+"/"+topic, jsonData, cfg.Retain)
	}
}

func PublishRelative(topic string, message any, retained bool) {
	PublishAbsolute(cfg.Topic+"/"+topic, message, cfg.Retain)
}

func SubscribeRelative(topic string, onMessage OnMessageListener) {
	Subscribe(cfg.Topic+"/"+topic, onMessage)
}

func Subscribe(topic string, onMessage OnMessageListener) {
	logger.Debug("Subscribing to topic", "topic", topic)
	callback := func(_ PAHO.Client, message PAHO.Message) {
		onMessage(message.Topic(), message.Payload())
	}

	subscriptionsMu.Lock()
	subscriptions = append(subscriptions, subscription{topic: topic, callback: callback})
	subscriptionsMu.Unlock()

	client.Subscribe(topic, cfg.QoS, callback)
}
