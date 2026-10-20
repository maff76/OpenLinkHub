# iCUE profile diagnostic parser

This is the first, read-only stage of the iCUE `.cueprofile` importer. It does not control LEDs.

Run from the OpenLinkHub source tree:

```bash
go run ./cmd/icueprofile-inspect /path/to/profile.cueprofile > profile-diagnostic.json
```

The diagnostic preserves:

- profile name and ID
- iCUE device entries (device ID, USB VID/PID, HID capabilities)
- channel index
- layer name, ID and enabled state
- LED selection mode (`AllLeds`, `ConcreteLeds`, etc.)
- explicit LED names for concrete selections
- cereal polymorphic lighting type, including references where `polymorphic_name` is omitted
- common effect parameters (duration, brightness, opacity, speed, direction, colors and transitions)

The parser deliberately keeps all device entries, including generic/default entries. Later importer stages can select the device topology that matches the active OpenLinkHub hardware.

## Halo-MasterChief validation

Against `LG_Halo-MasterChief_UDQ4lhx.cueprofile`, the parser finds 43 device entries, 156 channels and 374 lighting layers. The iCUE LINK System Hub entry (USB VID 6940 / PID 3135) contains three enabled layers on channel 0:

1. `Halo FX Visor` — `StaticLighting`, `ConcreteLeds`, 134 explicit LED IDs.
2. `Halo FX Rain` — `RainLighting`, `AllLeds`.
3. `Halo FX Green` — `StaticLighting`, `AllLeds`.

This is diagnostic-only. No assumptions about layer compositing or remapping a downloaded profile's device topology to the current machine are made yet.
