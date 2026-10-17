package icueprofile

import (
	"log"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RGB is one rendered LED colour before OpenLinkHub's device brightness limit.
type RGB struct{ R, G, B float64 }

// LEDPatch is one layer's command for one physical LED at the current profile
// time. Active=false means this layer does not touch the LED. Strength is kept
// separate from activity so fades do not turn into accidental ownership gaps.
type LEDPatch struct {
	Active   bool
	Color    RGB
	Strength float64
	Time     float64 // effect timeline timestamp; <0 means current renderer time
}

type LEDCommand struct {
	Time       float64
	LED        int
	Color      RGB
	Strength   float64
	LayerIndex int
	Effect     string
}

// persistentLEDState is the experimental v12.4.47 physical-LED state model.
// A rendered LED keeps its last committed RGB value until an effect issues a
// new command for that LED. In particular, StaticLighting seeds the state when
// a profile starts; it is not reasserted on every renderer frame.
type layerCommandState struct {
	active []bool
	color  []RGB
}

type persistentLEDState struct {
	leds   []RGB
	layers map[int]*layerCommandState
}

type persistentStateKey struct {
	started       int64
	deviceOrdinal int
	ledCount      int
}

// Key persistent state by the profile-run start timestamp rather than *Profile.
// The caller may materialize/copy Profile values between frames; the run start
// is the stable identity of one lighting execution.
var persistentLEDStates = struct {
	sync.Mutex
	byRun map[persistentStateKey]*persistentLEDState
}{byRun: make(map[persistentStateKey]*persistentLEDState)}

func persistentKey(deviceOrdinal, ledCount int, started time.Time) persistentStateKey {
	return persistentStateKey{started: started.UnixNano(), deviceOrdinal: deviceOrdinal, ledCount: ledCount}
}

func loadPersistentLEDState(deviceOrdinal, ledCount int, started time.Time) ([]RGB, bool) {
	persistentLEDStates.Lock()
	defer persistentLEDStates.Unlock()
	key := persistentKey(deviceOrdinal, ledCount, started)
	state := persistentLEDStates.byRun[key]
	if state == nil {
		persistentLEDStates.byRun[key] = &persistentLEDState{leds: make([]RGB, ledCount), layers: make(map[int]*layerCommandState)}
		return make([]RGB, ledCount), false
	}
	out := make([]RGB, ledCount)
	copy(out, state.leds)
	return out, true
}

func storePersistentLEDState(deviceOrdinal int, started time.Time, leds []RGB) {
	persistentLEDStates.Lock()
	defer persistentLEDStates.Unlock()
	key := persistentKey(deviceOrdinal, len(leds), started)
	state := persistentLEDStates.byRun[key]
	if state == nil {
		state = &persistentLEDState{layers: make(map[int]*layerCommandState)}
		persistentLEDStates.byRun[key] = state
	}
	state.leds = append(state.leds[:0], leds...)
}

// edgeTriggeredPatches turns a layer's rendered activity into explicit state
// changes. Remaining active with the same colour is NO COMMAND. Becoming
// active, or changing colour while active, is SET RGB. Becoming inactive is
// also NO COMMAND: it never reveals/rebuilds a lower layer.
func edgeTriggeredPatches(deviceOrdinal, ledCount int, started time.Time, layerIndex int, patches []LEDPatch) []LEDPatch {
	persistentLEDStates.Lock()
	defer persistentLEDStates.Unlock()
	key := persistentKey(deviceOrdinal, ledCount, started)
	state := persistentLEDStates.byRun[key]
	if state == nil {
		state = &persistentLEDState{leds: make([]RGB, ledCount), layers: make(map[int]*layerCommandState)}
		persistentLEDStates.byRun[key] = state
	}
	if state.layers == nil {
		state.layers = make(map[int]*layerCommandState)
	}
	prev := state.layers[layerIndex]
	if prev == nil || len(prev.active) != ledCount {
		prev = &layerCommandState{active: make([]bool, ledCount), color: make([]RGB, ledCount)}
		state.layers[layerIndex] = prev
	}
	out := make([]LEDPatch, ledCount)
	for i := 0; i < ledCount && i < len(patches); i++ {
		cur := patches[i]
		if cur.Active {
			// Compare the actual command colour after command strength. This keeps
			// continuous fades alive while suppressing repeated identical commands.
			cmd := applyPatch(RGB{}, cur)
			if !prev.active[i] || !rgbCommandNear(prev.color[i], cmd, 0.5) {
				out[i] = LEDPatch{Active: true, Color: cmd, Strength: 1, Time: -1}
			}
			prev.active[i] = true
			prev.color[i] = cmd
		} else {
			prev.active[i] = false
		}
	}
	return out
}

// colorWaveCommandTime returns the exact profile-timeline time at which the
// travelling ColorWave front most recently reached this LED.  v12.4.52 uses
// this instead of stamping the command with the renderer frame time, so a
// ColorWave note can correctly interrupt Rain/Wave/Gradient notes inside the
// same hardware-output interval.
func colorWaveCommandTime(l Layer, led, n int, elapsed float64, spatial []float64) (float64, bool) {
	speed := number(l.Parameters["speed"], 2)
	if speed < 1 {
		speed = 1
	}
	if speed > 3 {
		speed = 3
	}
	stepSeconds := 0.4 / speed
	if stepSeconds < 0.02 {
		stepSeconds = 0.02
	}
	direction := int(number(l.Parameters["direction"], 2))
	coord := float64(led)
	period := float64(maxInt(n, 1))
	if len(spatial) == n {
		coord = spatial[led]
		period = 17.0
	}
	routeCoord := coord
	if direction == 2 {
		routeCoord = math.Mod(period-coord, period)
	}
	phase := stringPhase(l.ID) * period
	front := elapsed/stepSeconds + phase
	cycle := math.Floor((front - routeCoord) / period)
	frontPos := routeCoord + cycle*period
	eventTime := (frontPos - phase) * stepSeconds
	if eventTime < -0.000001 || eventTime > elapsed+0.000001 {
		return 0, false
	}
	return eventTime, true
}

// rainCommandTime maps Rain's continuously fading drop state onto a canonical
// effect timeline rather than the wall-clock instant RenderFan happened to run.
// The 20 ms quantum is only a timestamp grid for the already-validated v12.4.38
// fade progression; it is not ownership and does not prevent another effect's
// timestamped command from interrupting Rain between Rain notes.
func rainCommandTime(elapsed float64) float64 {
	const q = 0.020
	if elapsed <= 0 {
		return 0
	}
	return math.Floor(elapsed/q) * q
}

func renderLayerPatches(l Layer, fanOrdinal, n int, elapsed float64, spatial []float64) ([]LEDPatch, bool) {
	colors, alpha, ok := renderLayer(l, fanOrdinal, n, elapsed, spatial)
	if !ok {
		return nil, false
	}
	patches := make([]LEDPatch, n)
	for i := 0; i < n && i < len(colors) && i < len(alpha); i++ {
		strength := alpha[i]
		if strength <= 0.000001 {
			continue
		}
		if strength > 1 {
			strength = 1
		}
		patches[i] = LEDPatch{Active: true, Color: colors[i], Strength: strength, Time: -1}
	}
	return patches, true
}

// gradientStateChangePatches converts an authored GradientLighting transition
// into explicit LED state-change commands. Between authored stop crossings the
// gradient issues NO COMMAND, so the physical LED retains whatever state was
// last committed by any layer. This deliberately does not change the authored
// duration, stop positions, colours, or profile clock.
func gradientStateChangePatches(l Layer, n int, elapsed float64) ([]LEDPatch, bool) {
	stops := transitionStops(l)
	if len(stops) < 2 {
		return nil, false
	}
	dur := number(l.Parameters["duration"], 1000) / 1000
	if dur <= 0 {
		dur = 1
	}
	const frameWindow = 0.025
	prevTime := elapsed - frameWindow
	if prevTime < 0 {
		prevTime = 0
	}
	var event *transitionStop
	eventAt := -1.0
	startCycle := int(math.Floor(prevTime / dur))
	endCycle := int(math.Floor(elapsed / dur))
	for cycle := startCycle; cycle <= endCycle; cycle++ {
		base := float64(cycle) * dur
		for si := range stops {
			st := &stops[si]
			if si == len(stops)-1 && st.T >= 0.999999 {
				continue
			}
			eventTime := base + st.T*dur
			if eventTime > prevTime && eventTime <= elapsed {
				event = st
				eventAt = eventTime
			}
		}
	}
	patches := make([]LEDPatch, n)
	if event == nil {
		return patches, true
	}
	strength := event.A
	if strength <= 0.000001 {
		return patches, true
	}
	if strength > 1 {
		strength = 1
	}
	layerOpacity := number(l.Parameters["opacity"], 1)
	if layerOpacity > 1 {
		layerOpacity /= 255
	}
	if layerOpacity < 0 {
		layerOpacity = 0
	}
	if layerOpacity > 1 {
		layerOpacity = 1
	}
	strength *= layerOpacity
	if strength <= 0.000001 {
		return patches, true
	}
	for i := range patches {
		patches[i] = LEDPatch{Active: true, Color: event.C, Strength: strength, Time: eventAt}
	}
	return patches, true
}

func applyPatch(dst RGB, patch LEDPatch) RGB {
	if !patch.Active {
		return dst
	}
	// Strength belongs to the active command itself. This retains the proven
	// fades used by Rain/Breathe while keeping inactivity as a separate state.
	a := patch.Strength
	return RGB{
		R: dst.R*(1-a) + patch.Color.R*a,
		G: dst.G*(1-a) + patch.Color.G*a,
		B: dst.B*(1-a) + patch.Color.B*a,
	}
}

// v12.4.47 persistent-state A/B retains the v12.4.46 command-clash audit. This is observational only: it does not alter
// rendering. A clash means two or more layers issue an active command to the
// same physical LED on the same renderer tick. A conflicting clash means those
// commands request visibly different RGB values.
type commandClashStats struct {
	mu                sync.Mutex
	bucket            int64
	frames            int64
	commands          int64
	ledsWithCommand   int64
	clashLEDs         int64
	conflictingLEDs   int64
	maxCommandsPerLED int
	perLEDCommands    []int64
	perLEDClashes     []int64
	effectCommands    map[string]int64
	effectLEDSlots    map[string]int64
}

var commandClashAudit = map[int]*commandClashStats{}
var commandClashAuditMu sync.Mutex

func clashStatsFor(deviceOrdinal, ledCount int) *commandClashStats {
	commandClashAuditMu.Lock()
	defer commandClashAuditMu.Unlock()
	s := commandClashAudit[deviceOrdinal]
	if s == nil {
		s = &commandClashStats{effectCommands: make(map[string]int64), effectLEDSlots: make(map[string]int64)}
		commandClashAudit[deviceOrdinal] = s
	}
	if s.effectCommands == nil {
		s.effectCommands = make(map[string]int64)
	}
	if s.effectLEDSlots == nil {
		s.effectLEDSlots = make(map[string]int64)
	}
	if len(s.perLEDCommands) != ledCount {
		s.perLEDCommands = make([]int64, ledCount)
		s.perLEDClashes = make([]int64, ledCount)
	}
	return s
}

func rgbCommandNear(a, b RGB, eps float64) bool {
	return math.Abs(a.R-b.R) <= eps && math.Abs(a.G-b.G) <= eps && math.Abs(a.B-b.B) <= eps
}

var ledNumber = regexp.MustCompile(`(?i)Led_Cmd_Led(\d+)$`)

// v12.4.45 patch resolver: effects generate sparse LED patches against a shared
// profile clock. A patch has an explicit active bit, so "no command" can never
// be confused with RGB black. Layers are applied base-to-top using iCUE's
// serialized top-to-bottom stack order. Authored black remains an active patch.

// Gradient event diagnostics are intentionally observational only: they do not
// change the v12.4.15 rendering. They count authored transition boundaries that
// the ~20 ms render cadence crosses, so hardware logs can distinguish scheduler
// problems from compositor problems before we change Gradient semantics again.
var gradientDiag = struct {
	sync.Mutex
	bucket       int64
	evaluated    int
	crossed      int
	selectedLEDs int
	samples      []string
}{}

// outputDiag traces Gradient events through topology selection and into the final
// per-fan RGB buffer. It is diagnostic-only and does not alter rendered colours.
var outputDiag = struct {
	sync.Mutex
	bucket             int64
	crossed            int
	mappedLEDRefs      int
	changedByGradients int
	finalChangedLEDs   int
	previous           map[int][]byte
}{previous: map[int][]byte{}}

var contributionAudit = struct {
	sync.Mutex
	bucket int64
}{}

func rgbNear(a, b RGB, tolerance float64) bool {
	return math.Abs(a.R-b.R) <= tolerance && math.Abs(a.G-b.G) <= tolerance && math.Abs(a.B-b.B) <= tolerance
}

// topologyLogicalBase returns the exact logical base only when the topology can
// express one. It is used solely for the diagnostic authored-selection count;
// actual mapping continues to use selectedWithTopology unchanged.
func topologyLogicalBase(topology LEDTopology, deviceOrdinal int) int {
	if topology == nil {
		return -1
	}
	logical, ok := topology.LogicalLED(deviceOrdinal, 0)
	if !ok {
		return -1
	}
	return logical
}

func gradientCrossings(elapsed, dur float64, stops []transitionStop) int {
	if dur <= 0 || len(stops) < 2 {
		return 0
	}
	const frameWindow = 0.025
	prev := elapsed - frameWindow
	if prev < 0 {
		prev = 0
	}
	crossed := 0
	startCycle := int(math.Floor(prev / dur))
	endCycle := int(math.Floor(elapsed / dur))
	for cycle := startCycle; cycle <= endCycle; cycle++ {
		base := float64(cycle) * dur
		for si, st := range stops {
			if si == len(stops)-1 && st.T >= 0.999999 {
				continue
			}
			eventTime := base + st.T*dur
			if eventTime > prev && eventTime <= elapsed {
				crossed++
			}
		}
	}
	return crossed
}

func diagnoseGradientEvents(l Layer, elapsed, dur float64, stops []transitionStop) {
	if dur <= 0 || len(stops) < 2 {
		return
	}
	// LSH normally renders every ~20 ms. Looking back 25 ms intentionally spans
	// one frame without requiring persistent per-layer state in the renderer.
	const frameWindow = 0.025
	prev := elapsed - frameWindow
	if prev < 0 {
		prev = 0
	}
	bucket := int64(math.Floor(elapsed))
	crossed := 0
	var sample string
	startCycle := int(math.Floor(prev / dur))
	endCycle := int(math.Floor(elapsed / dur))
	for cycle := startCycle; cycle <= endCycle; cycle++ {
		base := float64(cycle) * dur
		for si, st := range stops {
			// The final 1.0 stop is the next cycle's 0.0 boundary; count it once.
			if si == len(stops)-1 && st.T >= 0.999999 {
				continue
			}
			eventTime := base + st.T*dur
			if eventTime > prev && eventTime <= elapsed {
				crossed++
				if sample == "" {
					sample = l.ID + " dur=" + strconv.FormatFloat(dur, 'f', 3, 64) +
						" stop=" + strconv.FormatFloat(st.T, 'f', 3, 64) +
						" leds=" + strconv.Itoa(len(l.LEDs))
				}
			}
		}
	}

	gradientDiag.Lock()
	defer gradientDiag.Unlock()
	if gradientDiag.bucket == 0 {
		gradientDiag.bucket = bucket
	}
	if bucket != gradientDiag.bucket {
		log.Printf("iCUE Gradient diagnostic: evaluated=%d crossedEvents=%d selectedLEDRefs=%d samples=%v",
			gradientDiag.evaluated, gradientDiag.crossed, gradientDiag.selectedLEDs, gradientDiag.samples)
		gradientDiag.bucket = bucket
		gradientDiag.evaluated = 0
		gradientDiag.crossed = 0
		gradientDiag.selectedLEDs = 0
		gradientDiag.samples = gradientDiag.samples[:0]
	}
	gradientDiag.evaluated++
	gradientDiag.crossed += crossed
	gradientDiag.selectedLEDs += len(l.LEDs)
	if crossed > 0 && sample != "" && len(gradientDiag.samples) < 6 {
		gradientDiag.samples = append(gradientDiag.samples, sample)
	}
}

// LinkDevice returns the imported lighting entry for the iCUE LINK System Hub.
func (p *Profile) LinkDevice() *Device {
	for i := range p.Devices {
		if p.Devices[i].USBVendorID == 6940 && p.Devices[i].USBProductID == 3135 && len(p.Devices[i].Channels) > 0 {
			return &p.Devices[i]
		}
	}
	return nil
}

// DRAMDevice returns the most compact software-DRAM layout in the profile.
// iCUE exports several compatibility layouts; the smallest Led_Cmd canvas is
// the best match for current OpenLinkHub DIMMs and avoids applying large
// Dominator compatibility canvases to Vengeance modules.
func (p *Profile) DRAMDevice() *Device {
	var best *Device
	bestMax := int(^uint(0) >> 1)
	for i := range p.Devices {
		d := &p.Devices[i]
		if !strings.EqualFold(d.HIDCaps, "DramSoftware") || len(d.Channels) == 0 {
			continue
		}
		maxID := -1
		for _, ch := range d.Channels {
			for _, l := range ch.Layers {
				for _, key := range l.LEDs {
					m := ledNumber.FindStringSubmatch(strings.TrimSpace(key))
					if len(m) == 2 {
						n, _ := strconv.Atoi(m[1])
						if n > maxID {
							maxID = n
						}
					}
				}
			}
		}
		if maxID >= 0 && maxID < bestMax {
			best, bestMax = d, maxID
		}
	}
	return best
}

// RenderDRAM renders one physical DIMM against the imported software-DRAM
// canvas. OpenLinkHub reports VENGEANCE RGB RS DDR5 as six LEDs per DIMM.
// Halo's compact DRAM compatibility entry spans 0..17 in six-LED groups, so
// moduleOrdinal*ledCount is the proven selection mapping for this kit.
//
// Keep rendering on the established canvas path for now. The v10 experiment
// moved DRAM through spatial topology rendering and caused the physical RGB
// output to go black; that path is intentionally removed until its behaviour
// can be isolated independently from the logical LED mapping.
func (p *Profile) RenderDRAM(moduleOrdinal, ledCount int, started time.Time, brightness float64) ([]byte, []string) {
	d := p.DRAMDevice()
	if d == nil || len(d.Channels) == 0 {
		return make([]byte, ledCount*3), []string{"DRAM device entry missing"}
	}
	return renderCanvas(p.expandLightingLinks(d.Channels[0].Layers), moduleOrdinal*ledCount, ledCount, moduleOrdinal, started, brightness)
}

// RenderLINKDevice renders a non-QX LINK device. AllLeds layers are exact and
// therefore safely portable (Halo green + rain). ConcreteLeds are applied only
// when a known logical base is supplied; callers can pass -1 when the source
// profile's topology cannot be mapped reliably to the local device.
func (p *Profile) RenderLINKDevice(logicalBase, deviceOrdinal, ledCount int, started time.Time, brightness float64) ([]byte, []string) {
	d := p.LinkDevice()
	if d == nil || len(d.Channels) == 0 {
		return make([]byte, ledCount*3), []string{"LINK device entry missing"}
	}
	return renderCanvas(p.expandLightingLinks(d.Channels[0].Layers), logicalBase, ledCount, deviceOrdinal, started, brightness)
}

func (p *Profile) expandLightingLinks(layers []Layer) []Layer {
	if len(p.LightingLinkLayers) == 0 {
		return layers
	}
	out := make([]Layer, 0, len(layers)+len(p.LightingLinkLayers))
	for _, l := range layers {
		if !strings.EqualFold(l.Effect, "LightingLink") {
			out = append(out, l)
			continue
		}
		// The device LightingLink layer is a wrapper around the profile-level
		// LightingLinkProperty. The captured Sci-Fi profile references that shared
		// property rather than serializing another independent effect here.
		for _, linked := range p.LightingLinkLayers {
			copyLayer := linked
			copyLayer.Enabled = l.Enabled && linked.Enabled
			copyLayer.SelectionMode = l.SelectionMode
			copyLayer.LEDs = append([]string(nil), l.LEDs...)
			out = append(out, copyLayer)
		}
	}
	return out
}

func renderCanvas(layers []Layer, logicalBase, ledCount, ordinal int, started time.Time, brightness float64) ([]byte, []string) {
	if ledCount <= 0 {
		return []byte{}, nil
	}
	out := make([]RGB, ledCount)
	unsupported := map[string]bool{}
	elapsed := time.Since(started).Seconds()
	for li := len(layers) - 1; li >= 0; li-- {
		l := layers[li]
		if !l.Enabled {
			continue
		}
		patches, ok := renderLayerPatches(l, ordinal, ledCount, elapsed, nil)
		if !ok {
			unsupported[l.Effect] = true
			continue
		}
		for i := 0; i < ledCount; i++ {
			if !selectedAt(l, logicalBase, i) {
				continue
			}
			out[i] = applyPatch(out[i], patches[i])
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

func selectedAt(l Layer, logicalBase, local int) bool {
	if strings.EqualFold(l.SelectionMode, "AllLeds") || l.SelectionMode == "" {
		return true
	}
	if logicalBase < 0 {
		return false
	}
	logical := logicalBase + local
	for _, key := range l.LEDs {
		m := ledNumber.FindStringSubmatch(strings.TrimSpace(key))
		if len(m) == 2 {
			n, _ := strconv.Atoi(m[1])
			if n == logical {
				return true
			}
		}
	}
	return false
}

// v12.4.53 score compiler/player. Effects are recorded first, merged into one
// master score, then playback only consumes timestamped physical-LED notes.
type compiledScore struct {
	duration    float64
	notes       []LEDCommand
	unsupported []string
	perLayer    map[int]int
}

type scorePlayerState struct {
	score       *compiledScore
	leds        []RGB
	lastPos     float64
	lastElapsed float64
	started     bool
	logBucket   int64
}

var scorePlayers = struct {
	sync.Mutex
	byRun map[persistentStateKey]*scorePlayerState
}{byRun: make(map[persistentStateKey]*scorePlayerState)}

func concreteCommand(col RGB, strength float64) RGB {
	if strength < 0 {
		strength = 0
	}
	if strength > 1 {
		strength = 1
	}
	return RGB{R: col.R * strength, G: col.G * strength, B: col.B * strength}
}

func layerScoreDuration(l Layer) float64 {
	// Captured iCUE layers consistently expose duration in milliseconds where it
	// is present. It defines a candidate track length, not a live-render timeout.
	if v := number(l.Parameters["duration"], 0) / 1000; v > 0 {
		return v
	}
	return 0
}

func masterScoreDuration(layers []Layer) float64 {
	longest := 0.0
	for _, l := range layers {
		if !l.Enabled {
			continue
		}
		if d := layerScoreDuration(l); d > longest {
			longest = d
		}
	}
	if longest <= 0 {
		longest = 1
	}
	return longest
}

func appendScoreNote(dst *[]LEDCommand, l Layer, li, led int, at float64, col RGB, strength float64) {
	if at < -0.0000005 {
		return
	}
	*dst = append(*dst, LEDCommand{Time: at, LED: led, Color: concreteCommand(col, strength), Strength: 1, LayerIndex: li, Effect: l.Effect})
}

func recordLayerTrack(l Layer, li, fanOrdinal, ledCount int, duration float64, topology LEDTopology, sourceDeviceCount int) ([]LEDCommand, bool) {
	spatial := topologyPositions(topology, fanOrdinal, ledCount)
	notes := make([]LEDCommand, 0)
	effect := strings.ToLower(l.Effect)

	// Static is one opening chord. It never repaints until the whole score loops.
	if effect == "staticlighting" {
		colors, alpha, ok := renderLayer(l, fanOrdinal, ledCount, 0, spatial)
		if !ok {
			return nil, false
		}
		for led := 0; led < ledCount; led++ {
			if !selectedWithTopology(l, topology, fanOrdinal, led, sourceDeviceCount) {
				continue
			}
			if led < len(colors) && led < len(alpha) && alpha[led] > 0.000001 {
				appendScoreNote(&notes, l, li, led, 0, colors[led], alpha[led])
			}
		}
		return notes, true
	}

	// Gradient's authored transition stops are its notes. Record every stop in
	// every repetition that falls inside the master score; gaps are NO COMMAND.
	if effect == "gradientlighting" {
		stops := transitionStops(l)
		if len(stops) < 2 {
			return nil, false
		}
		dur := layerScoreDuration(l)
		if dur <= 0 {
			dur = 1
		}
		opacity := number(l.Parameters["opacity"], 1)
		if opacity > 1 {
			opacity /= 255
		}
		if opacity < 0 {
			opacity = 0
		}
		if opacity > 1 {
			opacity = 1
		}
		for base := 0.0; base < duration-0.0000005; base += dur {
			for si, st := range stops {
				if si == len(stops)-1 && st.T >= 0.999999 {
					continue
				}
				at := base + st.T*dur
				if at < -0.0000005 || at >= duration-0.0000005 {
					continue
				}
				for led := 0; led < ledCount; led++ {
					if !selectedWithTopology(l, topology, fanOrdinal, led, sourceDeviceCount) {
						continue
					}
					appendScoreNote(&notes, l, li, led, at, st.C, st.A*opacity)
				}
			}
		}
		return notes, true
	}

	// ColorWave records the exact instant each travelling front reaches each LED.
	// The effect is evaluated just after that instant to obtain the front colour.
	if effect == "colorwavelighting" {
		speed := number(l.Parameters["speed"], 2)
		if speed < 1 {
			speed = 1
		}
		if speed > 3 {
			speed = 3
		}
		step := 0.4 / speed
		if step < 0.02 {
			step = 0.02
		}
		period := float64(maxInt(ledCount, 1))
		if len(spatial) == ledCount {
			period = 17
		}
		phase := stringPhase(l.ID) * period
		direction := int(number(l.Parameters["direction"], 2))
		for led := 0; led < ledCount; led++ {
			if !selectedWithTopology(l, topology, fanOrdinal, led, sourceDeviceCount) {
				continue
			}
			coord := float64(led)
			if len(spatial) == ledCount {
				coord = spatial[led]
			}
			route := coord
			if direction == 2 {
				route = math.Mod(period-coord, period)
			}
			k0 := int(math.Floor((phase-route)/period)) - 1
			k1 := int(math.Ceil((duration/step+phase-route)/period)) + 1
			for k := k0; k <= k1; k++ {
				at := (route + float64(k)*period - phase) * step
				if at < -0.0000005 || at >= duration-0.0000005 {
					continue
				}
				colors, alpha, ok := renderLayer(l, fanOrdinal, ledCount, math.Max(0, at+0.000001), spatial)
				if ok && led < len(colors) && led < len(alpha) && alpha[led] > 0.000001 {
					appendScoreNote(&notes, l, li, led, math.Max(0, at), colors[led], alpha[led])
				}
			}
		}
		return notes, true
	}

	// Rain's v12.4.38 drop progression and Wave's moving tail are already proven
	// visual generators. For the score architecture test, record their explicit
	// output notes on the existing 20 ms LINK command grid. Repeated same-colour
	// notes are deliberately retained: another track may have changed that LED
	// between them, so they are meaningful opportunities to call it back.
	// Other supported continuous effects use the same recorder until their exact
	// authored note cadence is decoded.
	const q = 0.020
	seenSupported := false
	for at := 0.0; at < duration-0.0000005; at += q {
		colors, alpha, ok := renderLayer(l, fanOrdinal, ledCount, at, spatial)
		if !ok {
			continue
		}
		seenSupported = true
		for led := 0; led < ledCount; led++ {
			if !selectedWithTopology(l, topology, fanOrdinal, led, sourceDeviceCount) {
				continue
			}
			if led >= len(colors) || led >= len(alpha) || alpha[led] <= 0.000001 {
				continue
			}
			appendScoreNote(&notes, l, li, led, at, colors[led], alpha[led])
		}
	}
	return notes, seenSupported
}

func logScoreTimingCandidate(li int, l Layer, topology LEDTopology, fanOrdinal, ledCount int) {
	effect := strings.ToLower(l.Effect)
	switch effect {
	case "colorwavelighting":
		speed := number(l.Parameters["speed"], 2)
		if speed < 1 {
			speed = 1
		}
		if speed > 3 {
			speed = 3
		}
		step := 0.4 / speed
		period := float64(maxInt(ledCount, 1))
		if len(topologyPositions(topology, fanOrdinal, ledCount)) == ledCount {
			period = 17
		}
		log.Printf("iCUE SCORE TIMING: xml=%d effect=%s speed=%.3f ledStep=%.6fs routePeriod=%.3f frontRepeat=%.6fs serializedDuration=%.6fs", li, l.Effect, speed, step, period, step*period, layerScoreDuration(l))
	case "wavelighting":
		velocity := number(l.Parameters["velocity"], 100)
		if velocity == 0 {
			velocity = 100
		}
		tail := number(l.Parameters["tailLength"], 4)
		if tail <= 0 {
			tail = 1
		}
		coords := topologyPositions(topology, fanOrdinal, ledCount)
		if len(coords) != ledCount {
			coords = make([]float64, ledCount)
			for i := range coords {
				coords[i] = float64(i)
			}
		}
		span := float64(maxInt(ledCount, 1))
		if len(coords) > 0 {
			minC, maxC := coords[0], coords[0]
			for _, x := range coords[1:] {
				if x < minC {
					minC = x
				}
				if x > maxC {
					maxC = x
				}
			}
			span = maxC - minC + 1
		}
		natural := (span + tail) / math.Abs(velocity)
		log.Printf("iCUE SCORE TIMING: xml=%d effect=%s velocity=%.3f tail=%.3f naturalRepeat=%.6fs serializedDuration=%.6fs", li, l.Effect, velocity, tail, natural, layerScoreDuration(l))
	default:
		log.Printf("iCUE SCORE TIMING: xml=%d effect=%s serializedDuration=%.6fs", li, l.Effect, layerScoreDuration(l))
	}
}

func compileMasterScore(p *Profile, topology LEDTopology, deviceOrdinal, ledCount int) *compiledScore {
	d := p.LinkDevice()
	if d == nil || len(d.Channels) == 0 {
		return &compiledScore{duration: 1, unsupported: []string{"LINK device entry missing"}, perLayer: make(map[int]int)}
	}
	layers := p.expandLightingLinks(d.Channels[0].Layers)
	duration := masterScoreDuration(layers)
	sourceDeviceCount := sourceQXFanCount(layers, topology)
	score := &compiledScore{duration: duration, perLayer: make(map[int]int)}
	unsupported := map[string]bool{}
	for li, l := range layers {
		if !l.Enabled {
			continue
		}
		track, ok := recordLayerTrack(l, li, deviceOrdinal, ledCount, duration, topology, sourceDeviceCount)
		if !ok {
			unsupported[l.Effect] = true
			continue
		}
		score.perLayer[li] = len(track)
		score.notes = append(score.notes, track...)
	}
	// Earlier timestamp first. For a true same-time/same-LED collision, bottom
	// layers execute first and xml0/topmost executes last.
	sort.SliceStable(score.notes, func(i, j int) bool {
		a, b := score.notes[i], score.notes[j]
		if math.Abs(a.Time-b.Time) > 0.0000005 {
			return a.Time < b.Time
		}
		if a.LED != b.LED {
			return a.LED < b.LED
		}
		return a.LayerIndex > b.LayerIndex
	})
	for name := range unsupported {
		score.unsupported = append(score.unsupported, name)
	}
	sort.Strings(score.unsupported)
	if deviceOrdinal == 0 {
		log.Printf("iCUE SCORE COMPILED: duration=%.6fs layers=%d notes=%d", score.duration, len(layers), len(score.notes))
		for li, l := range layers {
			if l.Enabled {
				log.Printf("iCUE SCORE TRACK: xml=%d effect=%s notes=%d candidateDuration=%.6fs", li, l.Effect, score.perLayer[li], layerScoreDuration(l))
				logScoreTimingCandidate(li, l, topology, deviceOrdinal, ledCount)
			}
		}
		trueTies := 0
		for i := 1; i < len(score.notes); i++ {
			a, b := score.notes[i-1], score.notes[i]
			if a.LED == b.LED && math.Abs(a.Time-b.Time) <= 0.0000005 {
				trueTies++
			}
		}
		log.Printf("iCUE SCORE MERGE: inputNotes=%d outputNotes=%d trueSameTimeSameLEDTies=%d", len(score.notes), len(score.notes), trueTies)
	}
	return score
}

func applyScoreRange(state *scorePlayerState, from, to float64, includeZero bool) (int, int) {
	consumed := 0
	changed := map[int]struct{}{}
	for _, note := range state.score.notes {
		if note.Time > to+0.0000005 {
			break
		}
		eligible := note.Time > from+0.0000005
		if includeZero && note.Time >= -0.0000005 && note.Time <= to+0.0000005 {
			eligible = true
		}
		if !eligible {
			continue
		}
		if note.LED < 0 || note.LED >= len(state.leds) {
			continue
		}
		state.leds[note.LED] = note.Color
		consumed++
		changed[note.LED] = struct{}{}
	}
	return consumed, len(changed)
}

// RenderFan is now only the score player. Effect algorithms run while compiling
// the score; playback never asks Rain/Wave/ColorWave/Gradient what they look like.
func (p *Profile) RenderFan(topology LEDTopology, deviceOrdinal, ledCount int, started time.Time, brightness float64) ([]byte, []string) {
	key := persistentKey(deviceOrdinal, ledCount, started)
	scorePlayers.Lock()
	state := scorePlayers.byRun[key]
	if state == nil {
		state = &scorePlayerState{score: compileMasterScore(p, topology, deviceOrdinal, ledCount), leds: make([]RGB, ledCount)}
		scorePlayers.byRun[key] = state
	}
	elapsed := time.Since(started).Seconds()
	dur := state.score.duration
	if dur <= 0 {
		dur = 1
	}
	pos := math.Mod(elapsed, dur)
	if pos < 0 {
		pos += dur
	}
	consumed, changed := 0, 0
	from := state.lastPos
	wrapped := false
	if !state.started {
		from = 0
		consumed, changed = applyScoreRange(state, -1, pos, true)
		state.started = true
	} else if pos+0.0000005 < state.lastPos || elapsed-state.lastElapsed >= dur {
		wrapped = true
		c1, ch1 := applyScoreRange(state, state.lastPos, dur, false)
		c2, ch2 := applyScoreRange(state, -1, pos, true)
		consumed, changed = c1+c2, ch1+ch2
	} else {
		consumed, changed = applyScoreRange(state, state.lastPos, pos, false)
	}
	state.lastPos, state.lastElapsed = pos, elapsed
	bucket := int64(math.Floor(elapsed))
	if deviceOrdinal == 0 && bucket != state.logBucket {
		log.Printf("iCUE SCORE PLAYBACK: elapsed=%.6f from=%.6f to=%.6f/%.6f wrapped=%t notesConsumed=%d ledTouches=%d scoreNotes=%d", elapsed, from, pos, dur, wrapped, consumed, changed, len(state.score.notes))
		if consumed > len(state.score.notes) {
			log.Printf("iCUE SCORE INVARIANT FAILURE: notesConsumed=%d scoreNotes=%d", consumed, len(state.score.notes))
		}
		state.logBucket = bucket
	}
	leds := append([]RGB(nil), state.leds...)
	unsupported := append([]string(nil), state.score.unsupported...)
	scorePlayers.Unlock()

	if brightness <= 0 {
		brightness = 1
	}
	buf := make([]byte, 0, ledCount*3)
	for _, c := range leds {
		buf = append(buf, byte(clamp(c.R*brightness)), byte(clamp(c.G*brightness)), byte(clamp(c.B*brightness)))
	}
	return buf, unsupported
}

func selectedWithTopology(l Layer, topology LEDTopology, deviceOrdinal, local, sourceDeviceCount int) bool {
	if strings.EqualFold(l.SelectionMode, "AllLeds") || l.SelectionMode == "" {
		return true
	}
	if topology == nil {
		return false
	}
	mappedOrdinal := deviceOrdinal
	if sourceDeviceCount > 0 && deviceOrdinal >= sourceDeviceCount {
		mappedOrdinal = deviceOrdinal % sourceDeviceCount
	}
	logical, ok := topology.LogicalLED(mappedOrdinal, local)
	if !ok {
		return false
	}
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

// sourceQXFanCount returns the contiguous source QX fan span that is fully
// represented by ConcreteLeds in the selected iCUE LINK compatibility entry.
// QX uses 34 active LEDs in a 42-ID stride. Requiring complete 34/34 union
// coverage makes this conservative: sparse intentional selections are left
// exact, while effect families that partition every source fan (Electricity's
// staggered Gradient groups are the reference case) can be repeated safely onto
// additional local QX fans. IDs at 756+ are the measured TITAN 280 map.
func sourceQXFanCount(layers []Layer, topology LEDTopology) int {
	if topology == nil || !strings.EqualFold(topology.Name(), "QX") {
		return 0
	}
	covered := map[int]map[int]bool{}
	for _, l := range layers {
		if strings.EqualFold(l.SelectionMode, "AllLeds") || l.SelectionMode == "" {
			continue
		}
		for _, key := range l.LEDs {
			m := ledNumber.FindStringSubmatch(strings.TrimSpace(key))
			if len(m) != 2 {
				continue
			}
			id, err := strconv.Atoi(m[1])
			if err != nil || id < 0 || id >= 756 {
				continue
			}
			fan := id / 42
			local := id % 42
			if local >= 34 { // QX stride gap, not an active fan LED.
				continue
			}
			if covered[fan] == nil {
				covered[fan] = map[int]bool{}
			}
			covered[fan][local] = true
		}
	}
	count := 0
	for len(covered[count]) == 34 {
		count++
	}
	return count
}

func stringPhase(s string) float64 {
	// FNV-1a style deterministic phase seed. This is not randomness; it keeps
	// independent authored layers from collapsing onto the exact same event slot.
	var h uint32 = 2166136261
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return float64(h%10000) / 10000.0
}

func renderLayer(l Layer, fanOrdinal, n int, elapsed float64, spatial []float64) ([]RGB, []float64, bool) {
	if n <= 0 {
		return []RGB{}, []float64{}, true
	}
	c := make([]RGB, n)
	a := make([]float64, n)
	for i := range a {
		a[i] = 1
	}
	effect := strings.ToLower(l.Effect)

	// iCUE serializes execution separately from effect parameters. A layer that
	// only starts from a key event must not be rendered as a continuously running
	// profile layer. We currently have no input-event source in the LINK software
	// renderer, so leave reactive-only layers inactive rather than inventing
	// continuous playback. Continuous layers (startWithProfile=true) are unchanged.
	if l.StartOnKeyPress && !l.StartWithProfile {
		for i := range a {
			a[i] = 0
		}
		return c, a, true
	}

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
		applyLayerOpacity(l, a)
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
	case "breathelighting":
		// iCUE Breathe stores a single authored colour and an exact cycle duration.
		// Fade the layer alpha 0->1->0 while preserving the authored colour.
		col := layerColor(l, RGB{R: 255, G: 255, B: 255})
		dur := number(l.Parameters["duration"], 2000) / 1000
		if dur <= 0 {
			dur = 2
		}
		pulse := 0.5 - 0.5*math.Cos(elapsed/dur*2*math.Pi)
		for i := range c {
			c[i] = col
			a[i] *= pulse
		}
		applyLayerOpacity(l, a)
		return c, a, true
	case "rainlighting":
		// v12.4.38 A/B: model imported iCUE Rain as discrete short-lived drops
		// rather than a continuously travelling full-field waveform.  The cadence
		// and lifetime follow OpenLinkHub's proven native Rain timing model; iCUE's
		// authored colours, selection, direction, random flag and opacity remain in
		// force.  Authored duration is deliberately not used as a spatial sweep
		// period in this A/B.
		c1, c2 := effectColors(l, RGB{R: 25, G: 47, B: 9}, RGB{R: 100, G: 169, B: 45})
		speed := number(l.Parameters["speed"], 1)
		if speed <= 0 {
			speed = 1
		}
		random := boolParam(l.Parameters["random"], false)
		direction := int(number(l.Parameters["direction"], 0))
		stepDuration := 0.115 / speed
		dropDuration := 0.55 / math.Sqrt(speed)
		currentSlot := int(math.Floor(elapsed / stepDuration))
		period := float64(maxInt(n, 1))
		if len(spatial) == n {
			period = 17.0
		}
		for i := 0; i < n; i++ {
			coord := float64(i)
			if len(spatial) == n {
				coord = spatial[i]
			}
			best := 0.0
			bestMix := 0.0
			// Six recent spawn slots is the same bounded history used by native
			// OpenLinkHub Rain. Each slot is a discrete event that fades out.
			for back := 0; back < 6; back++ {
				slot := currentSlot - back
				if slot < 0 {
					continue
				}
				spawnTime := float64(slot) * stepDuration
				age := elapsed - spawnTime
				if age < 0 || age >= dropDuration {
					continue
				}
				// Deterministic Rain advances event positions through the logical
				// field; random Rain scatters them reproducibly instead.
				var target float64
				if random {
					target = math.Floor(hash(slot, fanOrdinal+31) * period)
				} else {
					target = math.Mod(float64(slot), period)
				}
				if direction == 1 || direction == 3 {
					target = math.Mod(period-target, period)
				}
				d := math.Abs(coord - target)
				d = math.Min(d, period-d)
				if d > 0.55 {
					continue
				}
				norm := age / dropDuration
				intensity := math.Pow(1-norm, 1.9)
				if intensity > best {
					best = intensity
					bestMix = hash(slot, i+17)
				}
			}
			if best <= 0 {
				a[i] = 0
				continue
			}
			c[i] = mixRGB(c1, c2, bestMix)
			a[i] *= best
		}
		applyLayerOpacity(l, a)
		return c, a, true
	case "colorwavelighting":
		// v12.4.45: ColorWave is a timed LED-command generator, not an opaque
		// full-layer framebuffer. Every layer runs on the shared profile clock
		// with its own speed/direction/colours. LEDs outside the travelling active
		// train return alpha zero (NO COMMAND), allowing lower layers/base to show.
		// This is deliberately the same architectural lesson that fixed Rain.
		c1, c2 := effectColors(l, RGB{R: 255}, RGB{B: 255})
		speed := number(l.Parameters["speed"], 2)
		if speed < 1 {
			speed = 1
		}
		if speed > 3 {
			speed = 3
		}
		direction := int(number(l.Parameters["direction"], 2))
		sign := 1.0
		if direction == 2 {
			sign = -1
		}

		// Keep the proven fixed renderer cadence independent of effect speed.
		// The discrete iCUE speed level changes progression through the LED route,
		// not output FPS. Use the serialized duration as the command lifetime when
		// present (the controlled RX test stores 1000 ms), while progression is a
		// continuous route clock. The reference speed=2 advances one LED about
		// every 200 ms; slow/fast scale progression, not frame cadence.
		lifetime := number(l.Parameters["duration"], 1000) / 1000
		if lifetime <= 0 {
			lifetime = 1
		}
		stepSeconds := 0.4 / speed
		if stepSeconds < 0.02 {
			stepSeconds = 0.02
		}
		random := boolParam(l.Parameters["random"], false)
		for i := 0; i < n; i++ {
			coord := float64(i)
			period := float64(maxInt(n, 1))
			if len(spatial) == n {
				coord = spatial[i]
				period = 17.0
			}
			// Determine when the current travelling front most recently reached
			// this LED. Several neighbouring LEDs remain commanded because lifetime
			// is intentionally longer than the per-LED progression interval.
			routeCoord := coord
			if sign < 0 {
				routeCoord = math.Mod(period-coord, period)
			}
			// Separate ColorWave layers are independent streams. Starting every
			// stream at route position zero makes otherwise valid patches land on
			// the same LEDs and lets the base dominate the rest of the fan. Give each
			// authored layer a stable route phase while preserving its own clock,
			// speed and direction.
			front := elapsed/stepSeconds + stringPhase(l.ID)*period
			cyclesBehind := math.Floor((front - routeCoord) / period)
			lastPass := routeCoord + cyclesBehind*period
			age := (front - lastPass) * stepSeconds
			if age < 0 || age >= lifetime {
				a[i] = 0 // NO COMMAND: retain the physical LED state unchanged.
				continue
			}

			// Colour advances as a sequence of fronts. Random mode uses stable
			// deterministic hues; alternating mode uses the two authored colours.
			passIndex := int(math.Floor(lastPass / period))
			if random {
				h := hash(passIndex, fanOrdinal*131+17)
				c[i] = hsv(h, 1, 1)
			} else if passIndex&1 == 0 {
				c[i] = c1
			} else {
				c[i] = c2
			}
			a[i] = 1
		}
		applyLayerOpacity(l, a)
		return c, a, true
	case "wavelighting":
		// Wave captures velocity, tailLength and isDoublesided. Do not assign a
		// physical unit to velocity here: the profile proves the value, not its unit.
		// Captured profiles also carry an authored transition
		// curve. Render that curve along the moving tail. Angle is retained by the
		// parser but is intentionally not interpreted on the 1-D LINK topology.
		velocity := number(l.Parameters["velocity"], 100)
		if velocity == 0 {
			velocity = 100
		}
		// Wave motion is driven directly by the authored velocity. Do not wrap
		// elapsed time at the serialized layer duration: the moving head already
		// wraps naturally at span+tail below. This A/B deliberately treats
		// duration as layer metadata rather than a second motion/repeat clock.
		waveElapsed := elapsed
		tail := number(l.Parameters["tailLength"], 4)
		if tail <= 0 {
			tail = 1
		}
		doubleSided := boolParam(l.Parameters["isDoublesided"], false)
		stops := transitionStops(l)
		if len(stops) == 0 {
			stops = []transitionStop{{0, RGB{R: 255, G: 255, B: 255}, 0}, {1, RGB{R: 255, G: 255, B: 255}, 1}}
		}
		coords := spatial
		if len(coords) != n {
			coords = make([]float64, n)
			for i := range coords {
				coords[i] = float64(i)
			}
		}
		minC, maxC := coords[0], coords[0]
		for _, x := range coords[1:] {
			if x < minC {
				minC = x
			}
			if x > maxC {
				maxC = x
			}
		}
		span := maxC - minC + 1
		head := math.Mod(waveElapsed*velocity, span+tail)
		for i, x := range coords {
			d := head - (x - minC)
			for d < 0 {
				d += span + tail
			}
			if d > tail {
				if doubleSided {
					d2 := (span + tail) - d
					if d2 >= 0 && d2 <= tail {
						d = d2
					} else {
						a[i] = 0
						continue
					}
				} else {
					a[i] = 0
					continue
				}
			}
			t := d / tail
			col, alpha := sampleTransitionStops(stops, t)
			c[i] = col
			a[i] *= alpha
		}
		applyLayerOpacity(l, a)
		return c, a, true
	case "colorshiftlighting":
		// Capture-derived structure: two authored colours, speed 1..3 and an
		// optional random flag. Color Shift is a whole-selection transition, not a
		// spatial wave, so every selected LED receives the same interpolated colour.
		c1, c2 := effectColors(l, RGB{R: 255}, RGB{B: 255})
		speed := discreteSpeed(l)
		period := capturedPeriod(l, 4.0/math.Pow(2, speed-1))
		t := (math.Sin(elapsed/period*2*math.Pi-math.Pi/2) + 1) / 2
		col := mixRGB(c1, c2, t)
		for i := range c {
			c[i] = col
		}
		applyLayerOpacity(l, a)
		return c, a, true
	case "colorpulselighting":
		// Color Pulse serializes the same two-colour/speed tuple as Color Shift.
		// Render a smooth A->B->A pulse; unlike Color Shift the midpoint dwells at
		// peak colour through a cosine intensity curve rather than moving spatially.
		c1, c2 := effectColors(l, RGB{R: 255}, RGB{B: 255})
		speed := discreteSpeed(l)
		period := capturedPeriod(l, 4.0/math.Pow(2, speed-1))
		pulse := 0.5 - 0.5*math.Cos(elapsed/period*2*math.Pi)
		col := mixRGB(c1, c2, pulse)
		for i := range c {
			c[i] = col
		}
		applyLayerOpacity(l, a)
		return c, a, true
	case "rainbowwavelighting":
		// Captures expose speed and direction only. Use the known topology
		// coordinate where available and retain a sequential fallback elsewhere.
		speed := discreteSpeed(l)
		period := capturedPeriod(l, 4.0/math.Pow(2, speed-1))
		direction := int(number(l.Parameters["direction"], 0))
		sign := 1.0
		if direction == 1 || direction == 3 {
			sign = -1
		}
		phase := sign * elapsed / period
		for i := 0; i < n; i++ {
			coord := float64(i)
			span := float64(maxInt(n, 1))
			if len(spatial) == n {
				coord = spatial[i]
				span = 17.0
			}
			c[i] = hsv(coord/span+phase, 1, 1)
		}
		applyLayerOpacity(l, a)
		return c, a, true
	case "watercolorlighting":
		// WaterColor captures two authored colours, speed, isRandom and a
		// background colour. Blend broad, slowly moving colour pools rather than
		// inventing discrete flashes; deterministic hashing keeps playback stable.
		c1, c2 := effectColors(l, RGB{R: 255}, RGB{B: 255})
		bg := layerParameterColor(l, "background", RGB{R: 255, G: 255, B: 255})
		speed := discreteSpeed(l)
		phase := elapsed * (0.20 * speed)
		random := boolParam(l.Parameters["isRandom"], false)
		for i := 0; i < n; i++ {
			coord := float64(i) / float64(maxInt(n, 1))
			if len(spatial) == n {
				coord = spatial[i] / 17.0
			}
			w1 := 0.5 + 0.5*math.Sin(2*math.Pi*(coord*1.7+phase))
			w2 := 0.5 + 0.5*math.Sin(2*math.Pi*(coord*0.9-phase*0.63+0.31))
			if random {
				w2 = 0.65*w2 + 0.35*hash(int(math.Floor(phase*4)), fanOrdinal*131+i)
			}
			pool := mixRGB(c1, c2, w1)
			c[i] = mixRGB(bg, pool, 0.35+0.65*w2)
		}
		applyLayerOpacity(l, a)
		return c, a, true
	case "rainbowlighting":
		// Rainbow is a whole-selection hue cycle (no direction field in captures).
		speed := discreteSpeed(l)
		period := capturedPeriod(l, 4.0/math.Pow(2, speed-1))
		col := hsv(elapsed/period, 1, 1)
		for i := range c {
			c[i] = col
		}
		applyLayerOpacity(l, a)
		return c, a, true
	case "rainbowchannellighting":
		// Rainbow Channel carries speed+direction and distributes the spectrum
		// across the channel, so it shares Rainbow Wave's spatial basis.
		speed := discreteSpeed(l)
		period := capturedPeriod(l, 4.0/math.Pow(2, speed-1))
		direction := int(number(l.Parameters["direction"], 0))
		sign := 1.0
		if direction == 1 || direction == 3 {
			sign = -1
		}
		phase := sign * elapsed / period
		for i := 0; i < n; i++ {
			coord := float64(i)
			span := float64(maxInt(n, 1))
			if len(spatial) == n {
				coord = spatial[i]
				span = 17.0
			}
			c[i] = hsv(coord/span+phase, 1, 1)
		}
		applyLayerOpacity(l, a)
		return c, a, true
	case "watercolorspectrumlighting":
		// Spectrum variant has no authored endpoint colours. Build overlapping
		// low-saturation spectrum pools from the captured speed/direction tuple.
		speed := discreteSpeed(l)
		direction := int(number(l.Parameters["direction"], 0))
		sign := 1.0
		if direction == 1 || direction == 3 {
			sign = -1
		}
		phase := sign * elapsed * (0.12 * speed)
		for i := 0; i < n; i++ {
			coord := float64(i) / float64(maxInt(n, 1))
			if len(spatial) == n {
				coord = spatial[i] / 17.0
			}
			h := coord*0.55 + phase + 0.08*math.Sin(2*math.Pi*(coord+phase*0.4))
			c[i] = hsv(h, 0.68, 1)
		}
		applyLayerOpacity(l, a)
		return c, a, true
	case "spiralrainbowlighting":
		speed := number(l.Parameters["speed"], 2)
		if speed <= 0 {
			speed = 2
		}
		period := capturedPeriod(l, 2.0*math.Pow(2, 2-speed))
		phase := elapsed / period
		clockwise := boolParam(l.Parameters["clockwise"], true)
		if !clockwise {
			phase = -phase
		}
		for i := 0; i < n; i++ {
			coord := float64(i)
			span := float64(maxInt(n, 1))
			if len(spatial) == n {
				coord = spatial[i]
				span = 17.0
			}
			hue := math.Mod(coord/span+phase, 1)
			if hue < 0 {
				hue += 1
			}
			c[i] = hsv(hue, 1, 1)
		}
		applyLayerOpacity(l, a)
		return c, a, true
	case "gradientlighting":
		// GradientLighting carries an authored transition curve.  Electricity, for
		// example, uses cyan -> pale blue -> very dark blue -> black -> cyan.
		// Falling back to the old generic green/orange endpoints made those layers
		// visibly green/yellow on LINK fans even though those colours do not exist
		// in the profile.  Follow the captured stops directly when present.
		dur := number(l.Parameters["duration"], 1000) / 1000
		if dur <= 0 {
			dur = 1
		}
		if stops := transitionStops(l); len(stops) >= 2 {
			diagnoseGradientEvents(l, elapsed, dur, stops)
			t := math.Mod(elapsed/dur, 1)
			if t < 0 {
				t += 1
			}
			col, alpha := sampleTransitionStops(stops, t)
			for i := range c {
				c[i] = col
				a[i] *= alpha
			}
			applyLayerOpacity(l, a)
			return c, a, true
		}

		// Retain the legacy two-colour fallback for older profiles that do not
		// serialize a transition curve.
		c1, c2 := gradientColors(l)
		t := (math.Sin(elapsed/dur*2*math.Pi) + 1) / 2
		col := mixRGB(c1, c2, t)
		for i := range c {
			c[i] = col
		}
		applyLayerOpacity(l, a)
		return c, a, true
	default:
		return c, a, false
	}
}

type transitionStop struct {
	T float64
	C RGB
	A float64
}

func transitionStops(l Layer) []transitionStop {
	v, ok := l.Parameters["transitions"]
	if !ok {
		return nil
	}
	xs, ok := v.([]string)
	if !ok {
		return nil
	}
	var out []transitionStop
	var t float64
	var haveT bool
	for _, raw := range xs {
		s := strings.TrimSpace(raw)
		if strings.HasPrefix(strings.ToLower(s), "time=") {
			if f, err := strconv.ParseFloat(strings.TrimSpace(strings.SplitN(s, "=", 2)[1]), 64); err == nil {
				t = f
				haveT = true
			}
			continue
		}
		if strings.HasPrefix(strings.ToLower(s), "color=") {
			if col, alpha, ok := parseTransitionColor(strings.TrimSpace(strings.SplitN(s, "=", 2)[1])); ok {
				if !haveT {
					t = float64(len(out))
				}
				out = append(out, transitionStop{T: t, C: col, A: alpha})
				haveT = false
			}
		}
	}
	// iCUE serializes Gradient transition times as authored normalized positions
	// within the layer duration. Preserve those timestamps exactly: re-scaling
	// them against the final stop can alter deliberately authored timing.
	return out
}

func parseTransitionColor(s string) (RGB, float64, bool) {
	low := strings.ToLower(strings.TrimSpace(s))
	// Captured iCUE gradients use both rgb(r g b) and rgba(r g b a), with
	// normalized 0..1 components.  Parse both forms before the generic colour
	// decoder so GradientLighting can consume its real transition palette.
	if strings.HasPrefix(low, "rgb(") || strings.HasPrefix(low, "rgba(") {
		isRGBA := strings.HasPrefix(low, "rgba(")
		prefix := "rgb("
		want := 3
		if isRGBA {
			prefix = "rgba("
			want = 4
		}
		inside := strings.TrimSuffix(strings.TrimPrefix(low, prefix), ")")
		parts := strings.Fields(inside)
		if len(parts) != want {
			return RGB{}, 0, false
		}
		vals := make([]float64, want)
		for i := range vals {
			f, e := strconv.ParseFloat(parts[i], 64)
			if e != nil {
				return RGB{}, 0, false
			}
			vals[i] = f
		}
		alpha := 1.0
		if isRGBA {
			alpha = vals[3]
		}
		return RGB{vals[0] * 255, vals[1] * 255, vals[2] * 255}, alpha, true
	}
	if c, ok := parseColor(s); ok {
		return c, 1, true
	}
	return RGB{}, 0, false
}

func sampleTransitionStops(stops []transitionStop, t float64) (RGB, float64) {
	if len(stops) == 0 {
		return RGB{}, 0
	}
	if t <= stops[0].T {
		return stops[0].C, stops[0].A
	}
	for i := 1; i < len(stops); i++ {
		if t <= stops[i].T {
			a, b := stops[i-1], stops[i]
			u := 0.0
			if b.T > a.T {
				u = (t - a.T) / (b.T - a.T)
			}
			return mixRGB(a.C, b.C, u), a.A + (b.A-a.A)*u
		}
	}
	last := stops[len(stops)-1]
	return last.C, last.A
}

// topologyPositions asks the device topology for spatial coordinates. Unknown
// hardware returns nil, which keeps spatial effects on the safe sequential fallback.
func topologyPositions(topology LEDTopology, deviceOrdinal, ledCount int) []float64 {
	if topology == nil || topology.LEDCount() != ledCount {
		return nil
	}
	out := make([]float64, ledCount)
	for i := 0; i < ledCount; i++ {
		pos, ok := topology.Position(deviceOrdinal, i)
		if !ok {
			return nil
		}
		out[i] = pos
	}
	return out
}

func applyLayerOpacity(l Layer, alpha []float64) {
	opacity := number(l.Parameters["opacity"], 255)
	if opacity > 1 {
		opacity /= 255
	}
	if opacity < 0 {
		opacity = 0
	}
	if opacity > 1 {
		opacity = 1
	}
	for i := range alpha {
		alpha[i] *= opacity
	}
}

func capturedPeriod(l Layer, fallback float64) float64 {
	dur := number(l.Parameters["duration"], 0) / 1000
	if dur > 0 {
		return dur
	}
	if fallback <= 0 {
		return 1
	}
	return fallback
}

func discreteSpeed(l Layer) float64 {
	speed := number(l.Parameters["speed"], 2)
	if speed < 1 {
		return 1
	}
	if speed > 3 {
		return 3
	}
	return speed
}

func layerParameterColor(l Layer, key string, fallback RGB) RGB {
	if v, ok := l.Parameters[key]; ok {
		if c, ok := parseColor(v); ok {
			return c
		}
	}
	return fallback
}

func effectColors(l Layer, fallback1, fallback2 RGB) (RGB, RGB) {
	if v, ok := l.Parameters["colors"]; ok {
		var found []RGB
		switch xs := v.(type) {
		case []string:
			for _, x := range xs {
				if c, ok := parseColor(x); ok {
					found = append(found, c)
				}
			}
		default:
			if c, ok := parseColor(v); ok {
				found = append(found, c)
			}
		}
		if len(found) >= 2 {
			return found[0], found[1]
		}
		if len(found) == 1 {
			return found[0], found[0]
		}
	}
	return fallback1, fallback2
}

func mixRGB(a, b RGB, t float64) RGB {
	if t < 0 {
		t = 0
	}
	if t > 1 {
		t = 1
	}
	return RGB{a.R + (b.R-a.R)*t, a.G + (b.G-a.G)*t, a.B + (b.B-a.B)*t}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func layerColor(l Layer, fallback RGB) RGB {
	// StaticLighting in exported iCUE profiles normally stores its colour in
	// the first transition rather than a top-level color field. Prefer that
	// source so imported profiles keep their authored colours (for example
	// Halo's amber visor and dark-green base).
	for _, key := range []string{"transitions", "color", "colors"} {
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
	// iCUE also serializes ordinary layer colours as color=rgb(r g b) or
	// color=rgba(r g b a), using normalized 0..1 components. Static/Breathe
	// layers use this form too, so decode it generically rather than only inside
	// transition curves.
	if i := strings.Index(s, "rgb("); i >= 0 {
		end := strings.Index(s[i:], ")")
		if end >= 0 {
			inside := s[i+4 : i+end]
			parts := strings.Fields(inside)
			if len(parts) == 3 {
				vals := make([]float64, 3)
				ok := true
				for j := range vals {
					vals[j], _ = strconv.ParseFloat(parts[j], 64)
					if _, e := strconv.ParseFloat(parts[j], 64); e != nil {
						ok = false
					}
				}
				if ok {
					return RGB{vals[0] * 255, vals[1] * 255, vals[2] * 255}, true
				}
			}
		}
	}
	if i := strings.Index(s, "rgba("); i >= 0 {
		end := strings.Index(s[i:], ")")
		if end >= 0 {
			inside := s[i+5 : i+end]
			parts := strings.Fields(inside)
			if len(parts) == 4 {
				vals := make([]float64, 3)
				ok := true
				for j := range vals {
					var e error
					vals[j], e = strconv.ParseFloat(parts[j], 64)
					if e != nil {
						ok = false
					}
				}
				if ok {
					return RGB{vals[0] * 255, vals[1] * 255, vals[2] * 255}, true
				}
			}
		}
	}
	// Leaf collections use strings such as first=4279840521.  Accept the
	// value side as packed ARGB/RGB while ignoring the alpha byte.
	if eq := strings.LastIndex(s, "="); eq >= 0 && eq+1 < len(s) {
		raw := strings.TrimSpace(s[eq+1:])
		if x, e := strconv.ParseUint(raw, 10, 32); e == nil && x > 255 {
			return RGB{float64(x >> 16 & 255), float64(x >> 8 & 255), float64(x & 255)}, true
		}
	}
	// Corsair transitions are collected as strings such as
	// "time=0 color=#ffcc8815". Find an embedded hash colour and accept both
	// #RRGGBB and iCUE's common #AARRGGBB form (discarding alpha here because
	// layer opacity is composited separately).
	if i := strings.Index(s, "#"); i >= 0 {
		hex := s[i+1:]
		end := 0
		for end < len(hex) && ((hex[end] >= '0' && hex[end] <= '9') || (hex[end] >= 'a' && hex[end] <= 'f')) {
			end++
		}
		hex = hex[:end]
		if len(hex) >= 8 {
			hex = hex[len(hex)-6:]
		}
		if len(hex) >= 6 {
			if x, e := strconv.ParseUint(hex[:6], 16, 32); e == nil {
				return RGB{float64(x >> 16 & 255), float64(x >> 8 & 255), float64(x & 255)}, true
			}
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
