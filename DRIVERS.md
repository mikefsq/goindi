# Writing a goindi device

A Go device implements `server.Device` and is registered with `server.Server`.
The server handles XML framing, property enumeration, client connections, and
BLOB subscriptions. Your device defines properties, handles commands, and owns
hardware access.

For client usage and the conformance checker, see [README.md](README.md).

## Choose an adapter or implement Device

Use `mount.New` to expose an `lx200.Mount` as an INDI telescope and guider.
Its `MountFunc` returns the current hardware interface or an error when
unavailable. `WithOptics` supplies telescope dimensions in millimetres;
`WithGuideRate` supplies a fallback rate as a fraction of sidereal.

Use `ccd.New` for a camera implementing `ccd.Camera`. Its `CameraFunc` returns
the current camera or an error. Frame returns undebayered little-endian pixels
at the reported bit depth. Optional `GainController` and `Subframer` interfaces
add controls on connection. `OffsetController` adds an offset control only when
GainController is also implemented. See [camera capabilities](ccd/caps.go).

For another device type, implement:

```go
type Device interface {
    Name() string
    Properties() []*server.Property
    HandleNew(pub server.Publisher, name string, members []server.NewMember)
}
```

Name must be unique within the server and stable across restarts. Properties
returns the current property set, including dynamically added properties.
The [focuser example](server/example_device_test.go) shows a small implementation
with fake hardware.

## Define properties

Use constructors in [server/standard.go](server/standard.go) for standard
properties such as CONNECTION and DRIVER_INFO. Set DRIVER_INTERFACE bits to
match the capabilities your device exposes.

Use `server.NewProperty` for additional number, switch, text, light, or BLOB
vectors. Set labels, groups, permissions, numeric bounds, and switch rules before
publishing. New properties begin in Idle.

Property value and state methods use locks. Exported metadata and the Member
pointers supplied to NewProperty are not protected from direct writes; initialize
them before publication and avoid concurrent mutation. Several setter calls are
not a single atomic update, so synchronize related operations in your driver.

Keep Properties synchronized with dynamic additions and removals. `Publisher.Define`
and `Publisher.Delete` send protocol messages; they do not maintain your device's
property collection.

## Handle commands

HandleNew runs on a client's read loop. Move slow work into a goroutine and
protect shared state: different clients can call HandleNew concurrently.

The server rejects unknown properties, read-only writes, mismatched vector types,
malformed numbers, and numbers outside declared bounds when Min is less than Max.
It also rejects multiple On values for exclusive switch vectors. The driver must
still validate member names, command combinations, connection state, and hardware
limits. Direct calls to HandleNew bypass server validation.

Parse number members with `NewMember.Float`, which accepts decimal and
sexagesimal values. Check the boolean result before issuing a hardware command.
Use Property.SetSwitch to enforce the property's switch rule.

Publish Busy when an asynchronous operation starts, then publish its final state
and values. Use Alert and Publisher.Message for failures. Serialize operation
starts and keep abort commands available while busy. A client's timeout does not
stop work on the device.

Publisher.Update sends values and state to subscribed clients. Use Define when
the property structure changes and Delete when it disappears.

## Lifecycle and hosting

Implement `server.Starter` to run background work:

```go
func (d *Device) Start(ctx context.Context, pub server.Publisher)
```

The server calls Start in a goroutine at startup or when a device is added to a
running server. Honor ctx cancellation, stop workers, and release resources your
driver owns. There is no separate Device.Close hook; coordinate and wait for
hardware cleanup in your host when needed. Serve does not join device workers.

Register devices and serve:

```go
s := server.New(":7624", server.WithLogger(log.Printf))
if err := s.AddDevice(dev); err != nil {
    return err
}
return s.Serve(ctx)
```

AddDevice rejects duplicate names. Serve returns a listen or accept error, or
returns normally after cancellation. Enable per-message diagnostics with
`server.WithDebug(true)` alongside a logger.

## Send images

Define a BLOB property before sending payloads through Publisher.SendBLOB.
Clients must enable delivery; the server defaults to Never and applies per-client
subscription policies. BLOB definitions remain visible even when payloads are
disabled.

Use a format such as `.fits`. For compressed data, use a `.z` suffix and report
the uncompressed byte count. Publishers implementing `server.BLOBSizedSender`
accept that count directly; ordinary SendBLOB calculates it by inflation.
Slow clients are disconnected when their output queues fill.

## Tests

Test command validation and hardware behavior with a fake backend. Cover state
transitions, overlapping requests, cancellation, and failure cleanup. Then drive
the device over TCP with the client; see [server tests](server/server_test.go),
[mount tests](mount/mount_test.go), and [camera tests](ccd/ccd_test.go).

Run `go test ./...` and `go test -race ./...`. `conform.Run` can check your server
through a client connection; state-changing checks require `Options.Mutate`.

Optional integration tests start a real libindi server and simulators on a Unix
system. Point GOINDI_INDI_BUILD at the libindi build tree containing
`indiserver/indiserver` and the built simulator drivers:

```sh
GOINDI_INDI_BUILD=/path/to/indi/build go test -tags integration ./client
```

Without the variable, the tests look under `$HOME/indihurd/build/indi` and skip
if indiserver is absent. See the [integration fixture](client/indiserver_test.go)
for driver paths and shared-library setup.
