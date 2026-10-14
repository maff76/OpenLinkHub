package icueprofile

import "strings"

// LEDTopology describes device geometry independently from an iCUE profile or
// lighting effect. New fan families can be added here without changing Rain,
// Gradient, or the profile parser.
type LEDTopology interface {
	Name() string
	LEDCount() int
	Position(deviceOrdinal, localLED int) (float64, bool)
	LogicalLED(deviceOrdinal, localLED int) (int, bool)
}

type qxTopology struct{}

func (qxTopology) Name() string  { return "QX" }
func (qxTopology) LEDCount() int { return 34 }

func (qxTopology) Position(deviceOrdinal, localLED int) (float64, bool) {
	if localLED < 0 || localLED >= len(qxLocalX) {
		return 0, false
	}
	const fanPitch = 365.0
	const ledPitch = 20.0
	return (float64(deviceOrdinal)*fanPitch + qxLocalX[localLED]) / ledPitch, true
}

func (qxTopology) LogicalLED(deviceOrdinal, localLED int) (int, bool) {
	if localLED < 0 || localLED >= len(qxLocalX) {
		return 0, false
	}
	// Verified from controlled iCUE exports: 34 active LEDs in a 42-id stride.
	return deviceOrdinal*42 + localLED, true
}

var qxLocalX = [...]float64{
	10, 30, 50, 70, 90, 110, 130, 150, 170, 190, 210, 230,
	50, 30, 10, 230, 210, 190, 170, 150, 130, 110, 90, 70,
	270, 280, 290, 310, 320, 330, 260, 320, 300, 280,
}

// titan280Topology is the verified active-LED geometry from the iCUE SDK
// capture for the user's iCUE LINK TITAN 280. The SDK exposes the TITAN as
// the final 20 active LEDs after nine 34-LED QX blocks. The coordinates below
// are normalized from the captured LINK-strip X positions so motion effects
// preserve Corsair's logical ordering without depending on the hub's absolute
// chain offset.
//
// The corresponding .cueprofile ConcreteLeds IDs are not yet proven, so
// LogicalLED deliberately returns false. AllLeds layers (including Halo Green
// and Halo Rain) still render with the verified TITAN spatial topology, while
// ConcreteLeds layers such as Halo Visor remain safely skipped on the TITAN.
type titan280Topology struct{}

func (titan280Topology) Name() string  { return "TITAN 280" }
func (titan280Topology) LEDCount() int { return 20 }

func (titan280Topology) Position(deviceOrdinal, localLED int) (float64, bool) {
	if localLED < 0 || localLED >= len(titan280LocalX) {
		return 0, false
	}
	const ledPitch = 20.0
	return titan280LocalX[localLED] / ledPitch, true
}

func (titan280Topology) LogicalLED(deviceOrdinal, localLED int) (int, bool) {
	return 0, false
}

// Captured SDK order 306..325, normalized by subtracting the minimum X (3315).
var titan280LocalX = [...]float64{
	80, 100, 120, 140, 160, 180, 200, 220, 240, 260, 280, 300,
	0, 20, 40, 60,
	380, 320, 340, 360,
}

// dram6Topology describes the 6-LED software-DRAM layout exported by iCUE as
// Dram1030. OpenLinkHub identifies VENGEANCE RGB RS DDR5 as a 6-LED DIMM, so
// this is the exact compatibility layout for that family rather than the old
// "smallest DRAM canvas" heuristic.
//
// Halo's Dram1030 entry proves the first two module ranges as 0..5 and 6..11:
// its Green layer selects both full ranges, while Rain 1 / Rain 2 split them
// into complementary even/odd masks. That is enough to map the user's two
// physical DIMMs without inventing any Windows-SDK dependency.
type dram6Topology struct{}

func (dram6Topology) Name() string  { return "DRAM 6 LED (Dram1030)" }
func (dram6Topology) LEDCount() int { return 6 }

func (dram6Topology) Position(deviceOrdinal, localLED int) (float64, bool) {
	if localLED < 0 || localLED >= 6 {
		return 0, false
	}
	// Keep modules contiguous in iCUE's software-DRAM canvas. Spatial effects
	// therefore continue naturally from DIMM 0 into DIMM 1.
	return float64(deviceOrdinal*6 + localLED), true
}

func (dram6Topology) LogicalLED(deviceOrdinal, localLED int) (int, bool) {
	if deviceOrdinal < 0 || localLED < 0 || localLED >= 6 {
		return 0, false
	}
	return deviceOrdinal*6 + localLED, true
}

// DRAMTopologyForLEDCount returns only DRAM layouts whose per-module LED count
// is known. Additional Corsair memory families can be added here as their iCUE
// software layouts are verified.
func DRAMTopologyForLEDCount(ledCount int) LEDTopology {
	switch ledCount {
	case 6:
		return dram6Topology{}
	default:
		return nil
	}
}

// TopologyForDevice returns only verified topology maps. Unknown families such
// as RX/LX deliberately return nil until captured, preventing a QX map from
// being applied to different hardware.
func TopologyForDevice(deviceName string, ledCount int) LEDTopology {
	name := strings.ToLower(deviceName)
	if strings.Contains(name, "qx") && ledCount == 34 {
		return qxTopology{}
	}
	if strings.Contains(name, "titan") && ledCount == 20 {
		return titan280Topology{}
	}
	return nil
}
