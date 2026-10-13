package icueprofile

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// RGB is one rendered LED colour before OpenLinkHub's device brightness limit.
type RGB struct{ R, G, B float64 }

var ledNumber = regexp.MustCompile(`(?i)Led_Cmd_Led(\d+)$`)

// LinkDevice returns the imported lighting entry for the iCUE LINK System Hub.
func (p *Profile) LinkDevice() *Device {
	for i := range p.Devices {
		if p.Devices[i].USBVendorID == 6940 && p.Devices[i].USBProductID == 3135 && len(p.Devices[i].Channels) > 0 {
			return &p.Devices[i]
		}
	}
	return nil
}

// RenderQX renders one QX fan from an imported LINK profile. Captured QX data
// establishes 34 physical LEDs in a 42-id logical stride. Layers are composited
// bottom-to-top (reverse XML order), matching iCUE's layer stack semantics.
func (p *Profile) RenderQX(fanOrdinal, ledCount int, started time.Time, brightness float64) ([]byte, []string) {
	out := make([]RGB, ledCount)
	unsupported := map[string]bool{}
	d := p.LinkDevice()
	if d == nil || len(d.Channels) == 0 {
		return make([]byte, ledCount*3), []string{"LINK device entry missing"}
	}
	layers := d.Channels[0].Layers
	elapsed := time.Since(started).Seconds()
	for li := len(layers) - 1; li >= 0; li-- {
		l := layers[li]
		if !l.Enabled {
			continue
		}
		colors, alpha, ok := renderLayer(l, fanOrdinal, ledCount, elapsed)
		if !ok {
			unsupported[l.Effect] = true
			continue
		}
		for i := 0; i < ledCount; i++ {
			if !selected(l, fanOrdinal, i) {
				continue
			}
			a := alpha[i]
			out[i].R = out[i].R*(1-a) + colors[i].R*a
			out[i].G = out[i].G*(1-a) + colors[i].G*a
			out[i].B = out[i].B*(1-a) + colors[i].B*a
		}
	}
	if brightness <= 0 {
		brightness = 1
	}
	buf := make([]byte, 0, ledCount*3)
	for _, c := range out {
		buf = append(buf, byte(clamp(c.R*brightness)), byte(clamp(c.G*brightness)), byte(clamp(c.B*brightness)))
	}
	names := make([]string, 0, len(unsupported))
	for n := range unsupported {
		names = append(names, n)
	}
	return buf, names
}

func selected(l Layer, fanOrdinal, local int) bool {
	if strings.EqualFold(l.SelectionMode, "AllLeds") || l.SelectionMode == "" {
		return true
	}
	logical := fanOrdinal*42 + local
	for _, key := range l.LEDs {
		m := ledNumber.FindStringSubmatch(strings.TrimSpace(key))
		if len(m) != 2 {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		if n == logical {
			return true
		}
	}
	return false
}

func renderLayer(l Layer, fanOrdinal, n int, elapsed float64) ([]RGB, []float64, bool) {
	c := make([]RGB, n)
	a := make([]float64, n)
	for i := range a {
		a[i] = 1
	}
	effect := strings.ToLower(l.Effect)
	switch effect {
	case "basicnonelighting":
		for i := range a {
			a[i] = 0
		}
		return c, a, true
	case "staticlighting":
		col := layerColor(l, RGB{R: 0, G: 255, B: 70})
		for i := range c {
			c[i] = col
		}
		return c, a, true
	case "blinklighting":
		col := layerColor(l, RGB{R: 255, G: 255, B: 255})
		dur := number(l.Parameters["duration"], 1000) / 1000
		if dur <= 0 {
			dur = 1
		}
		on := math.Mod(elapsed, dur) < dur/2
		if on {
			for i := range c {
				c[i] = col
			}
		} else {
			for i := range a {
				a[i] = 0
			}
		}
		return c, a, true
	case "rainlighting":
		col := layerColor(l, RGB{R: 30, G: 220, B: 70})
		speed := number(l.Parameters["speed"], 1)
		if speed <= 0 {
			speed = 1
		}
		slotDur := 0.12 / speed
		slot := int(elapsed / slotDur)
		for i := 0; i < n; i++ {
			h := hash(slot-i*2, fanOrdinal*42+i)
			if h > 0.76 {
				intensity := 0.35 + 0.65*hash(slot, i+17)
				c[i] = RGB{col.R * intensity, col.G * intensity, col.B * intensity}
			} else {
				a[i] = 0
			}
		}
		return c, a, true
	case "spiralrainbowlighting":
		speed := number(l.Parameters["speed"], 2)
		if speed <= 0 {
			speed = 2
		}
		period := 2.0 * math.Pow(2, 2-speed)
		phase := elapsed / period
		clockwise := boolParam(l.Parameters["clockwise"], true)
		if !clockwise {
			phase = -phase
		}
		for i := 0; i < n; i++ {
			hue := math.Mod(float64(fanOrdinal*42+i)/42.0+phase, 1)
			c[i] = hsv(hue, 1, 1)
		}
		return c, a, true
	case "gradientlighting":
		c1, c2 := gradientColors(l)
		dur := number(l.Parameters["duration"], 1000) / 1000
		if dur <= 0 {
			dur = 1
		}
		t := (math.Sin(elapsed/dur*2*math.Pi) + 1) / 2
		col := RGB{c1.R + (c2.R-c1.R)*t, c1.G + (c2.G-c1.G)*t, c1.B + (c2.B-c1.B)*t}
		for i := range c {
			c[i] = col
		}
		return c, a, true
	default:
		return c, a, false
	}
}

func layerColor(l Layer, fallback RGB) RGB {
	for _, key := range []string{"color", "colors"} {
		if v, ok := l.Parameters[key]; ok {
			if c, ok := parseColor(v); ok {
				return c
			}
		}
	}
	return fallback
}
func gradientColors(l Layer) (RGB, RGB) {
	if v, ok := l.Parameters["colors"]; ok {
		if xs, ok := v.([]string); ok {
			var cs []RGB
			for _, x := range xs {
				if c, ok := parseColor(x); ok {
					cs = append(cs, c)
				}
			}
			if len(cs) >= 2 {
				return cs[0], cs[len(cs)-1]
			}
		}
	}
	c := layerColor(l, RGB{R: 0, G: 255, B: 70})
	return c, RGB{R: 255, G: 180, B: 0}
}
func parseColor(v any) (RGB, bool) {
	s := strings.ToLower(strings.TrimSpace(toString(v)))
	if s == "" {
		return RGB{}, false
	}
	// Accept #RRGGBB and 0xRRGGBB/ARGB values.
	if strings.HasPrefix(s, "#") && len(s) >= 7 {
		if x, e := strconv.ParseUint(s[1:7], 16, 32); e == nil {
			return RGB{float64(x >> 16 & 255), float64(x >> 8 & 255), float64(x & 255)}, true
		}
	}
	if strings.HasPrefix(s, "0x") {
		if x, e := strconv.ParseUint(s[2:], 16, 32); e == nil {
			return RGB{float64(x >> 16 & 255), float64(x >> 8 & 255), float64(x & 255)}, true
		}
	}
	// cereal colour leaf lists commonly contain r=/g=/b= or red=/green=/blue=.
	var r, g, b float64
	var hr, hg, hb bool
	fields := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' || r == ' ' || r == '[' || r == ']' })
	for _, f := range fields {
		kv := strings.SplitN(f, "=", 2)
		if len(kv) != 2 {
			continue
		}
		x, e := strconv.ParseFloat(kv[1], 64)
		if e != nil {
			continue
		}
		switch kv[0] {
		case "r", "red":
			r = x
			hr = true
		case "g", "green":
			g = x
			hg = true
		case "b", "blue":
			b = x
			hb = true
		}
	}
	if hr && hg && hb {
		if r <= 1 && g <= 1 && b <= 1 {
			r *= 255
			g *= 255
			b *= 255
		}
		return RGB{r, g, b}, true
	}
	if x, e := strconv.ParseUint(strings.Trim(s, "[]"), 10, 32); e == nil && x > 255 {
		return RGB{float64(x >> 16 & 255), float64(x >> 8 & 255), float64(x & 255)}, true
	}
	return RGB{}, false
}
func toString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []string:
		return strings.Join(x, " ")
	default:
		return ""
	}
}
func number(v any, d float64) float64 {
	switch x := v.(type) {
	case int:
		return float64(x)
	case float64:
		return x
	case string:
		if f, e := strconv.ParseFloat(x, 64); e == nil {
			return f
		}
	}
	return d
}
func boolParam(v any, d bool) bool {
	switch x := v.(type) {
	case bool:
		return x
	case int:
		return x != 0
	case string:
		b, e := strconv.ParseBool(x)
		if e == nil {
			return b
		}
	}
	return d
}
func clamp(x float64) float64 {
	if x < 0 {
		return 0
	}
	if x > 255 {
		return 255
	}
	return x
}
func hash(a, b int) float64 {
	x := math.Sin(float64(a*127+b*311)+78.233) * 43758.5453
	return x - math.Floor(x)
}
func hsv(h, s, v float64) RGB {
	h = math.Mod(h, 1)
	if h < 0 {
		h += 1
	}
	i := int(h * 6)
	f := h*6 - float64(i)
	p := v * (1 - s)
	q := v * (1 - f*s)
	t := v * (1 - (1-f)*s)
	var r, g, b float64
	switch i % 6 {
	case 0:
		r, g, b = v, t, p
	case 1:
		r, g, b = q, v, p
	case 2:
		r, g, b = p, v, t
	case 3:
		r, g, b = p, q, v
	case 4:
		r, g, b = t, p, v
	case 5:
		r, g, b = v, p, q
	}
	return RGB{r * 255, g * 255, b * 255}
}
