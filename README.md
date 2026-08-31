# goindi

INDI server and client libraries for Go.

## Client

```go
c, err := client.Dial(ctx, "localhost:7624")
defer c.Close()
c.GetProperties("", "")
c.WaitDevices(1, 3*time.Second)

_, err = c.SetNumberAndWait("10Micron", "EQUATORIAL_EOD_COORD",
	map[string]float64{"RA": 5.5, "DEC": -5.4}, 10*time.Second)
```

## Server

```go
s := server.New(":7624", server.WithLogger(log.Printf))
s.AddDevice(dev) // anything implementing server.Device
go s.Serve(ctx)
```

## Conformance testing

`conform` validates any INDI server.

```
indiconform -addr localhost:7624                            # read-only, every device
indiconform -addr localhost:7624 -device 10Micron -mutate
```

## Go device drivers

`mount` and `ccd` are INDI drivers written in Go that `server` hosts directly,
without a driver binary. Tests run `conform` against them to prove the server serves a conforming device.

## Layout

```
server/   protocol and hub: property model, XML codec, sexagesimal numbers,
          the Device/Starter/Publisher interfaces, standard property
          constructors, DRIVER_INTERFACE bits
client/   INDI client: enumerate, set-and-wait, BLOB sinks, liveness
conform/  black-box conformance validator
cmd/indiconform/  CLI wrapper around conform
mount/    Go driver: telescope and guider over any lx200.Mount
ccd/      Go driver: camera over any frame source
```
