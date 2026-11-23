# mqtt-gateway

Internal helper module for all my go projects that use MQTT.

```
go get github.com/philipparndt/mqtt-gateway
```

## Connecting

`mqtt.Start` connects once and exits the process when the broker is not
reachable. `mqtt.StartContext` waits for the broker instead, retrying every
`mqtt.RetryInterval` (5 s) until it is connected or the context is cancelled:

```go
mqtt.MaxReconnectInterval = 5 * time.Second // default: 10 minutes
mqtt.DropWhenDisconnected = true            // default: publishes wait for the broker
mqtt.PublishTimeout = 10 * time.Second      // default: no limit
mqtt.SetOnReconnect(func() {
	// republish state that was dropped during the outage
})

if err := mqtt.StartContext(ctx, cfg, "my-gateway"); err != nil {
	return // ctx was cancelled before the broker was reachable
}
defer mqtt.Stop()
```

Set the options before starting. The defaults keep the previous behaviour.
