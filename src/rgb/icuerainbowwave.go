package rgb

import (
	"math"
	"time"
)

// ICUERainbowWave is an experimental software renderer for Corsair iCUE's
// Rainbow Wave effect. The initial implementation deliberately renders one
// device locally: QX fans expose 34 physical RGB LEDs in iCUE. Captured iCUE
// profiles show a 42-position logical stride between adjacent QX fans, but we
// do not apply that inter-device phase until the complete LINK device-to-canvas
// mapping has been verified.
//
// Captured iCUE defaults:
//
//	duration   = 1000
//	brightness = 10
//	speed      = 2
//	direction  = 2
//
// OpenLinkHub keeps its existing 1..3 speed convention and 4/5 horizontal
// direction convention in the profile UI. This first renderer is intended for
// direct visual comparison against iCUE on a single QX fan.
func (r *ActiveRGB) ICUERainbowWave(startTime time.Time) {
	speed := r.RgbModeSpeed
	if speed <= 0 {
		speed = 2
	}

	// Keep speed 2 at one full hue rotation per second, matching the captured
	// iCUE duration=1000 ms default. 1 is faster and 3 is slower.
	secondsPerCycle := 1.0
	switch {
	case speed <= 1:
		secondsPerCycle = 0.5
	case speed >= 3:
		secondsPerCycle = 2.0
	}

	phase := math.Mod(time.Since(startTime).Seconds()/secondsPerCycle, 1.0)
	if r.RgbDirection == 5 {
		phase = -phase
	}

	buf := make(map[int][]byte, r.LightChannels)
	channels := r.LightChannels
	if channels < 1 {
		r.Raw = buf
		r.Output = nil
		return
	}

	for i := 0; i < channels; i++ {
		// One complete rainbow is distributed across the physical LEDs of the
		// device. This is the value we will tune from the one-QX iCUE comparison.
		position := math.Mod((float64(i)/float64(channels))+phase, 1.0)
		if position < 0 {
			position += 1.0
		}

		hue := position * 360.0
		red, green, blue := hsvToRGB(hue, 1.0, 1.0)
		color := ModifyBrightness(Color{
			Red:        float64(red),
			Green:      float64(green),
			Blue:       float64(blue),
			Brightness: r.RGBBrightness,
		})

		if len(r.Buffer) > 0 {
			r.Buffer[i] = byte(color.Red)
			r.Buffer[i+r.ColorOffset] = byte(color.Green)
			r.Buffer[i+(r.ColorOffset*2)] = byte(color.Blue)
		} else {
			buf[i] = []byte{byte(color.Red), byte(color.Green), byte(color.Blue)}
			if r.IsAIO && r.HasLCD && i > 15 && i < 20 {
				buf[i] = []byte{0, 0, 0}
			}
		}
	}

	r.Raw = buf
	if r.Inverted {
		r.Output = SetColorInverted(buf)
	} else {
		r.Output = SetColor(buf)
	}
}
