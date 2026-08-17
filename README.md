# goindi

A lightwight Go **INDI server** to allow clients like PHD2 to communicate with 
a generic telescope and camera device. 

## Layout

```
goindi/
├── server/        the protocol + hub (stdlib only)
│   ├── property.go   Property/Member model, state/perm/rule
│   ├── xml.go        wire structs + marshal; INDI is a stream of top-level
│   │                 XML elements, no root, no length framing
│   ├── device.go     Device + Starter + Publisher interfaces
│   ├── standard.go   standard property constructors (telescope) + interface masks
│   └── server.go     Hub: accept loop, streaming xml.Decoder read loop,
│                     device registry keyed by name, broadcast fan-out
├── mount/         generic INDI telescope+guider over any lx200.Mount
├── ccd/           generic INDI camera over any frame source (CCD_INFO + FITS BLOB)
├── client/        a Go INDI client (connect, enumerate, set, wait)
├── conform/       the ConformU analogue for INDI — a black-box validator
└── cmd/indiconform/  CLI: point it at any INDI server, get a conformance report
```

## Devices

- **`mount`** — telescope+guider over any `lx200.Mount`: `EQUATORIAL_EOD_COORD`,
  `TELESCOPE_TIMED_GUIDE_NS/WE` (PHD2's pulse-guide), `TELESCOPE_INFO`, etc.
- **`ccd`** — camera over a `ccd.Camera` frame source: `CCD_INFO` (pixel size,
  geometry — what PHD2 reads to compute pixel scale), `CCD_EXPOSURE`, and the **`CCD1`
  BLOB** carrying each frame as **FITS**. It delivers **raw frames, no pixel
  transformation** (no debayer/bin/stretch) — the client centroids on the unmodified
  sensor data. BLOBs are gated by `enableBLOB` (sent only to clients that ask).

## Conformance testing (the ConformU analogue)

We do **not** depend on libindi's `indi_getprop`/`indi_setprop`. Instead `conform`
is a native validator: a Go INDI **client** (sharing no code with `server`) drives
any INDI server and reports protocol and standard-contract compliance.

```
indiconform -addr localhost:7624                 # validate every device
indiconform -addr localhost:7624 -device 10Micron -mutate=false
```

It checks: device discovery; `DRIVER_INFO`/`DRIVER_INTERFACE`; per-property state/
perm/switch-rule/number-range validity; the `CONNECTION` contract; the telescope
contract (`EQUATORIAL_EOD_COORD`, `ON_COORD_SET`, `TELESCOPE_ABORT_MOTION`) when the
TELESCOPE bit is set; the guider contract (`TELESCOPE_TIMED_GUIDE_NS/WE`) when the
GUIDER bit is set; and, with `-mutate`, behavioral checks (CONNECT settles `Ok`, the
OneOfMany switch invariant, a pulse guide is accepted). Exit status is non-zero on
any failure. The test suite runs it against the real mount device (must pass) and a
deliberately-broken device (must be caught).

## Ports & addressing — one endpoint, many devices

INDI multiplexes **devices behind one connection** by the `device="…"` attribute
on every message, so you do **not** allocate a port per device. Run one hub on the
conventional `:7624`, register many devices, and each is addressed by its **name**. 
INDI has **no discovery** (unlike Alpaca's UDP 32227), so the port is static and 
clients are pointed at `host:7624` + a device name.

```go
s := server.New(":7624", server.WithLogger(log.Printf))
s.AddDevice(mount.New("10Micron", tel.LiveMount)) // tel.LiveMount is a MountFunc
// later: s.AddDevice(ccd.New("Guide camera", …))
go s.Serve(ctx)
```

PHD2: **INDI Mount → localhost:7624 → device "10Micron"**; **INDI Camera →
localhost:7624 → "Guide camera"**. One port, pick by name, no `indiserver`.

## The mount device

`mount.New(name, MountFunc)` is **generic** — every goalpaca mount satisfies
`lx200.Mount`, so this one adapter serves them all. It exposes the standard 
telescope+guider property set and maps it onto the mount:

| INDI property | Mount action |
|---|---|
| `CONNECTION` | the `MountFunc` (live mount / not-connected) |
| `EQUATORIAL_EOD_COORD` + `ON_COORD_SET` | `SetTargetRA/Dec` → `SlewToTarget`/`SyncToTarget`, **under `OpLock`** |
| `TELESCOPE_TIMED_GUIDE_NS/WE` | `lx200.Guider.PulseGuide(dir, ms)` — the property PHD2 guides with |
| `TELESCOPE_ABORT_MOTION` | `Halt()` |
| `TELESCOPE_PIER_SIDE` | `lx200.PierSider` (read, polled) |
| `DRIVER_INFO` | `DRIVER_INTERFACE = TELESCOPE\|GUIDER (5)` |

State integrity matches the bridge: position (`EQUATORIAL_EOD_COORD`) is read
**live** from the mount on a poll, never cached; writes go through the shared
`OpLock`, so an INDI-driven slew and an Alpaca-driven slew can't interleave and
corrupt the target register. Pulse-guides are single `:Mg…#` commands, already
serialized by the mount's command mutex.

## alpacahurd integration

Both the mount and CCD devices are wired into the `alpacahurd` binary
(`goalpaca_devices`): an `indi` config block hosts one in-process `:7624` hub, and
each alpaca device opts into it (the device `name` becomes the INDI device id).
The CCD adapter drives any Alpaca camera as a frame source, so the **same** device
serves the simulator and the real ZWO guide camera (`astrocam`'s `PureASICamera`)
with no driver changes — PHD2 lists it as a guide camera over INDI.
