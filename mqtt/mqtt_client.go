package mqtt

import (
	"encoding/json"
	"expvar"
	"math/rand"
	"os"
	"sync"
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
)

const (
	statusOnline  = "online"
	statusOffline = "offline"
)

var client PAHO.Client
var cfg config.MQTTConfig

var connectionWg sync.WaitGroup

type OnMessageListener func(string, []byte)

type subscription struct {
	topic     string
	callback  PAHO.MessageHandler
}

var (
	subscriptions   []subscription
	subscriptionsMu sync.Mutex
)

func Start(config config.MQTTConfig, clientIdPrefix string) {
	connectionWg.Add(1)
	go connect(config, clientIdPrefix)
	connectionWg.Wait()
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

func connect(config config.MQTTConfig, clientIdPrefix string) {
	cfg = config
	statusTopic := cfg.Topic + "/bridge/state"
	clientID := clientIdPrefix + "_" + generateRandomClientID(10)
	logger.Debug("Generated client ID", "clientID", clientID)

	firstConnect := true

	opts := PAHO.NewClientOptions().
		AddBroker(config.URL).
		SetClientID(clientID).
		SetWill(statusTopic, statusOffline, 1, true)

	opts.Password = config.Password
	opts.Username = config.Username

	opts.SetOnConnectHandler(func(_ PAHO.Client) {
		if firstConnect {
			firstConnect = false
			mqttConnectedAt = time.Now()
			return
		}

		mqttReconnects++
		mqttLastReconnect = time.Now()
		logger.Info("Reconnected to MQTT broker, re-subscribing", "reconnects", mqttReconnects)

		PublishAbsolute(statusTopic, statusOnline, true)
		guardStatus(statusTopic)
		resubscribe()
	})

	opts.SetConnectionLostHandler(func(_ PAHO.Client, err error) {
		logger.Error("MQTT connection lost", "error", err)
	})

	client = PAHO.NewClient(opts)
	if token := client.Connect(); token.Wait() && token.Error() != nil {
		logger.Error("Error connecting to MQTT broker", "error", token.Error())
		os.Exit(1)
	}
	// No deferred Disconnect here: this function never returns (see select{}
	// below), so the defer was unreachable. Shutting down is Stop's job.

	// Always retained, whatever cfg.Retain says: the will is retained, so a
	// non-retained "online" left every late subscriber reading "offline".
	PublishAbsolute(statusTopic, statusOnline, true)
	guardStatus(statusTopic)

	logger.Info("Connected to MQTT broker", "url", config.URL)
	connectionWg.Done()
	go LogMessagesPublished()

	// Keep the connection active until the application is terminated
	select {}
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
	token := client.Publish(topic, cfg.QoS, retained, message)
	token.Wait()

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
