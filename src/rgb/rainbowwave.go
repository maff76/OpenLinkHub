package rgb

import (
	"math"
	"time"
)

// RainbowWave implements a spatial rainbow wave inspired by OpenRGB's
// Rainbow Wave effect. Frequency controls the hue separation between
// adjacent LEDs (1-50 degrees), while direction reverses LED traversal.
func (r *ActiveRGB) RainbowWave(startTime time.Time) {
	frequency := r.Frequency
	if frequency < 1 || frequency > 50 {
		frequency = 10
	}

	speed := r.RgbModeSpeed
	if speed <= 0 {
		speed = 2
	}

	// OpenRGB's default is Speed=40. Keep the OpenLinkHub 1..3 speed
	// convention while preserving approximately the same default motion.
	degreesPerSecond := 50.0 - (speed * 15.0) // 1=35, 2=20, 3=5
	if degreesPerSecond < 5 {
		degreesPerSecond = 5
	}
	progress := math.Mod(time.Since(startTime).Seconds()*degreesPerSecond, 360.0)

	buf := map[int][]byte{}
	for i := 0; i < r.LightChannels; i++ {
		led := i
		// Existing OpenLinkHub direction values use 4/5 for left/right.
		// Treat 5 as reverse; all other values retain forward ordering.
		if r.RgbDirection == 5 {
			led = (r.LightChannels - 1) - i
		}

		hue := math.Mod(progress+(float64(led)*frequency), 360.0)
		red, green, blue := hsvToRGB(hue, 1.0, 1.0)
		color := ModifyBrightness(Color{
			Red: float64(red), Green: float64(green), Blue: float64(blue), Brightness: r.RGBBrightness,
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
