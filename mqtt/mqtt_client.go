package mqtt

import (
	"encoding/json"
	"expvar"
	"math/rand"
	"os"
	"sync"
	"time"

	PAHO "github.com/eclipse/paho.mqtt.golang"
	"github.com/philipparndt/go-logger"
	"github.com/philipparndt/mqtt-gateway/config"
)

var messagesPublishedCtr int

var (
	LogPayloadTruncate      = true
	LogPayloadTruncateBytes = 100
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
		logger.Info("Messages published (last hour)", "count", messagesPublishedCtr)
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

func connect(config config.MQTTConfig, clientIdPrefix string) {
	cfg = config
	statusTopic := cfg.Topic + "/bridge/state"
	clientID := clientIdPrefix + "_" + generateRandomClientID(10)
	logger.Debug("Generated client ID", "clientID", clientID)

	firstConnect := true

	opts := PAHO.NewClientOptions().
		AddBroker(config.URL).
		SetClientID(clientID).
		SetWill(statusTopic, "offline", 1, true)

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

		PublishAbsolute(statusTopic, "online", cfg.Retain)
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
	defer client.Disconnect(250)

	PublishAbsolute(statusTopic, "online", cfg.Retain)

	logger.Info("Connected to MQTT broker", "url", config.URL)
	connectionWg.Done()
	go LogMessagesPublished()

	// Keep the connection active until the application is terminated
	select {}
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
		logger.Debug("Published message", "topic", topic, "message", message)
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
