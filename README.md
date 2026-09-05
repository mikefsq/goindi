# goindi

INDI client and server libraries for Go, with telescope and camera adapters and
a command-line conformance checker. Requires Go 1.25 or later.

## Use the client

Add the module to your application:

```sh
go get github.com/mikefsq/goindi
```

Connect to an INDI server and list its devices:

```go
package main

import (
    "context"
    "fmt"
    "log"
    "time"

    "github.com/mikefsq/goindi/client"
)

func main() {
    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()
    c, err := client.Dial(ctx, "localhost:7624")
    if err != nil {
        log.Fatal(err)
    }
    defer c.Close()

    if err := c.GetProperties("", ""); err != nil {
        log.Print(err)
        return
    }
    if !c.WaitDevices(1, 3*time.Second) {
        log.Print("No devices received before timeout or disconnection")
        return
    }
    for _, name := range c.Devices() {
        fmt.Println(name)
    }
}
```

Enumeration arrives asynchronously. `WaitDevices` waits for a minimum count;
more devices and properties can arrive afterwards. `Properties` and `Property`
return snapshots. The Dial context bounds connection establishment; call Close
to end the session. The client does not reconnect automatically; watch Done
and inspect Err before redialing.

Use `SetNumber`, `SetSwitch`, or `SetText` to send changes. The `AndWait` variants
wait for a newer property update in `Ok` or `Alert`, returning an error for Alert.
Some operations complete in `Idle`; use `WaitRev` with an appropriate predicate
for those. INDI has no command IDs, so an unrelated update can satisfy a wait.
Cancelling a wait does not cancel the device operation.

## Check an INDI server

Build the checker from a checkout:

```sh
go build -o indiconform ./cmd/indiconform
./indiconform -addr localhost:7624
```

The default run reads definitions and checks selected standard property
contracts. Limit the run to one device with `-device "Device Name"`.

State-changing checks are optional:

```sh
./indiconform -addr localhost:7624 -device "CCD Simulator" -mutate -timeout 60s
```

`-mutate` can connect devices, issue guide pulses, and take exposures. It attempts
to disconnect devices it connected. Use a simulator or a prepared hardware setup.
`-timeout` bounds the overall run (default 30 seconds).

Exit codes are 0 for no failed checks, 1 for failed checks, and 2 for a connection
error. Warnings do not cause a failing exit code. These checks cover selected
contracts, not every device feature.

## Write a device

See [DRIVERS.md](DRIVERS.md) for property definitions, command handling,
publication, lifecycle, and tests. The server hosts Go implementations of
`server.Device`; it does not launch external INDI driver binaries.

## Packages

| Package | Purpose |
|---------|---------|
| `client` | Enumeration, property updates, command waits, BLOBs, and FITS decoding |
| `server` | XML protocol, property model, and TCP device server |
| `mount` | Telescope and guider adapter over `lx200.Mount` |
| `ccd` | Camera adapter over a frame source |
| `conform` | Protocol and property-contract checks |

## Tests

```sh
go test ./...
go test -race ./...
```

Tests use local servers and fake hardware. Optional tests against libindi's
simulators are described in [DRIVERS.md](DRIVERS.md#tests).
