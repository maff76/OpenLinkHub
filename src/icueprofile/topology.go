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

// titan280Topology is the measured active-LED geometry and .cueprofile
// ConcreteLeds mapping for the iCUE LINK TITAN 280.
//
// Evidence chain:
//   - the SDK capture exposes 20 active TITAN LEDs after the nine 34-LED QX blocks;
//   - controlled iCUE exports map the 20 UI zones to Led_Cmd_Led756..775;
//   - a server-side one-output-at-a-time hardware test measured local outputs
//     0..19 as UI zones 6..16, 1..5, then 17..20 respectively.
//
// Combining those independent captures proves local output 0..19 maps to logical
// LED 756..775 in order. Keep this explicit table rather than deriving the value so
// future hardware/capture changes cannot silently turn a measured map into an
// assumption. For the four centre LEDs, the measured physical output order is
// top, right, bottom, left (logical LEDs 772, 773, 774, 775).
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
	if localLED < 0 || localLED >= len(titan280LogicalLEDs) {
		return 0, false
	}
	return titan280LogicalLEDs[localLED], true
}

// Measured OpenLinkHub output order -> iCUE ConcreteLeds logical ID.
var titan280LogicalLEDs = [...]int{
	756, 757, 758, 759, 760, 761, 762, 763, 764, 765,
	766, 767, 768, 769, 770, 771, 772, 773, 774, 775,
}

// Captured SDK order 306..325, normalized by subtracting the minimum X (3315).
var titan280LocalX = [...]float64{
	80, 100, 120, 140, 160, 180, 200, 220, 240, 260, 280, 300,
	0, 20, 40, 60,
	380, 320, 340, 360,
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
