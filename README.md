[![GoDoc](https://pkg.go.dev/badge/centrifugal/centrifuge-go)](https://pkg.go.dev/github.com/centrifugal/centrifuge-go)

Websocket client for [Centrifugo](https://github.com/centrifugal/centrifugo) server and [Centrifuge](https://github.com/centrifugal/centrifuge) library.

There is no v1 release of this library yet – API still evolves. At the moment patch version updates only contain backwards compatible changes, minor version updates can have backwards incompatible API changes.

Check out [client SDK API specification](https://centrifugal.dev/docs/transports/client_api) to learn how this SDK behaves. It's recommended to read that before starting to work with this SDK as the spec covers common SDK behavior - describes client and subscription state transitions, main options and methods. Also check out examples folder.

The features implemented by this SDK can be found in [SDK feature matrix](https://centrifugal.dev/docs/transports/client_sdk#sdk-feature-matrix).

## Callbacks should not block

When using this SDK you should not block for a long time inside event handlers since handlers called synchronously by the SDK and block the connection read loop. The fact that the read loop is blocked also means that you can not issue blocking `Client` requests such as `Publish`, `RPC`, `History`, `Presence`, `PresenceStats` from the event handler code – this will result into a deadlock. Use a separate goroutine if you really need to issue a blocking call from inside an event handler.

I.e. this code is broken:

```go
client.OnMessage(func(e centrifuge.MessageEvent) {
    result, err := c.RPC(context.Background(), "method", []byte("{}"))
    if err != nil {
        log.Println(err)
        return
    }
    // Will never be called.
    log.Printf("RPC result 2: %s", string(result.Data))
})
```

This code is correct as it does not block event handler forever:

```go
client.OnMessage(func(e centrifuge.MessageEvent) {
    // When issue blocking requests from inside event handler we must use
    // a goroutine. Otherwise, we will ge a deadlock since the connection
    // read loop is blocked.
    go func() {
        result, err := c.RPC(context.Background(), "method", []byte("{}"))
        if err != nil {
            log.Println(err)
            return
        }
        log.Printf("RPC result 2: %s", string(result.Data))
    }()
})
```

You can find similar limitations in [eclipse/paho.mqtt.golang](https://github.com/eclipse/paho.mqtt.golang#common-problems). In short, this is caused by a challenging mix of asynchronous protocol, Go and callback approach. In the previous versions of this SDK we allowed blocking requests from within event handlers – but it contradicts with the real-time nature of Centrifugal ecosystem, because we had to use separate  callback queue, and that queue could grow huge without a reasonable way to backpressure (which we were not able to find).

If you are calling `Publish`, `RPC`, `History`, `Presence`, `PresenceStats` from the outside of event handler – you should not do any special care.

## Run tests

First run Centrifugo instance:

```
docker run -d -p 8000:8000 centrifugo/centrifugo:latest centrifugo --client_insecure
```

Then run `go test`
