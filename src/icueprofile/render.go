package icueprofile

import (
	"fmt"
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

// RainCalibration exposes temporary, in-memory timing controls used to match
// iCUE's Slow/Normal/Fast Rain presets on real hardware. Values are spawn
// intervals in milliseconds. Event lifetime scales with the selected interval
// so changing timing does not also change Rain density/overlap.
type RainCalibration struct {
	SlowMS   float64 `json:"slowMs"`
	NormalMS float64 `json:"normalMs"`
	FastMS   float64 `json:"fastMs"`
}

var rainCalibration = struct {
	sync.RWMutex
	values RainCalibration
}{values: RainCalibration{SlowMS: 200, NormalMS: 100, FastMS: 200.0 / 3.0}}

func GetRainCalibration() RainCalibration {
	rainCalibration.RLock()
	defer rainCalibration.RUnlock()
	return rainCalibration.values
}

func SetRainCalibration(v RainCalibration) RainCalibration {
	// Keep the diagnostic control inside a useful/safe calibration range.
	clamp := func(x, fallback float64) float64 {
		if x <= 0 {
			return fallback
		}
		if x < 20 {
			return 20
		}
		if x > 1000 {
			return 1000
		}
		return x
	}
	current := GetRainCalibration()
	v.SlowMS = clamp(v.SlowMS, current.SlowMS)
	v.NormalMS = clamp(v.NormalMS, current.NormalMS)
	v.FastMS = clamp(v.FastMS, current.FastMS)
	rainCalibration.Lock()
	rainCalibration.values = v
	rainCalibration.Unlock()
	return v
}

func rainStepDuration(speed float64) float64 {
	v := GetRainCalibration()
	switch {
	case speed < 1.5:
		return v.SlowMS / 1000.0
	case speed < 2.5:
		return v.NormalMS / 1000.0
	default:
		return v.FastMS / 1000.0
	}
}

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
	stepSeconds := 0.2 / speed
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
		routeCoord = wrapPeriodic(period-coord, period)
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
	layers := p.expandLightingLinks(d.Channels[0].Layers)
	logicalBase := moduleOrdinal * ledCount
	logical := make([]int, ledCount)
	for local := 0; local < ledCount; local++ {
		logical[local] = logicalBase + local
	}
	log.Printf("iCUE LED TOPOLOGY AUDIT: topology=DRAM rendererOrdinal=%d leds=%d localToLogical=%v", moduleOrdinal, ledCount, logical)
	staticFrame := auditCanvasStaticOpeningFrame(layers, logicalBase, ledCount, moduleOrdinal)
	log.Printf("iCUE STATIC FRAME AUDIT: topology=DRAM rendererOrdinal=%d leds=%d preLiveRGB=%v", moduleOrdinal, ledCount, auditRGBHex(staticFrame))
	return renderCanvas(layers, logicalBase, ledCount, moduleOrdinal, started, brightness)
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

// v12.4.64 continuous live-performer concert sequencer.
//
// The profile is an open-ended performance: selecting a profile starts a new
// concert at t=0 and changing/stopping the profile ends it. There is no master
// score length. Each effect performs its own finite phrase and repeats that phrase
// forever on the shared absolute clock. A phrase boundary never rewinds another
// effect. Static is the conductor's opening tap and is emitted only at concert start.
type compiledTrack struct {
	layerIndex int
	effect     string
	repeat     float64
	oneShot    bool
	live       bool
	layer      Layer
	notes      []LEDCommand
}

type compiledScore struct {
	tracks      []compiledTrack
	unsupported []string
	totalNotes  int
}

type scorePlayerState struct {
	score             *compiledScore
	topology          LEDTopology
	deviceOrdinal     int
	sourceDeviceCount int
	leds              []RGB
	lastElapsed       float64
	started           bool
	logBucket         int64
	visualSamples     []RGB
	visualSampleSet   []bool
	visualLogAt       []float64
	renderCalls       int
	renderDeltaSum    float64
	renderDeltaMin    float64
	renderDeltaMax    float64
	renderCadenceAt   float64
	orangeAuditAt     float64
	rainBase          [][]RGB
	rainLast          [][]RGB
	rainActive        [][]bool
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
	if v := number(l.Parameters["duration"], 0) / 1000; v > 0 {
		return v
	}
	return 0
}

func waveTrackRepeat(l Layer, topology LEDTopology, fanOrdinal, ledCount int) float64 {
	// The authored duration is the Wave performer's phrase/repeat period. Velocity
	// and Tail describe what happens inside that phrase; they do not define when
	// the whole concert or another effect repeats.
	if d := layerScoreDuration(l); d > 0 {
		return d
	}
	// Defensive fallback for profiles with no captured phrase time: use one natural
	// spatial pass rather than inventing a global/master duration.
	velocity := math.Abs(number(l.Parameters["velocity"], 100))
	if velocity <= 0.000001 {
		velocity = 100
	}
	tail := number(l.Parameters["tailLength"], 4)
	if tail <= 0 {
		tail = 1
	}
	coords := topologyPositions(topology, fanOrdinal, ledCount)
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
	r := (span + tail) / velocity
	if r <= 0.020 {
		r = 0.020
	}
	return r
}

// trackRepeatDuration deliberately keeps each effect's phrase clock separate.
// Serialized duration is used only where the guide/profile semantics support it.
func trackRepeatDuration(l Layer, topology LEDTopology, fanOrdinal, ledCount int) (float64, bool) {
	effect := strings.ToLower(l.Effect)
	switch effect {
	case "staticlighting":
		return 0, true
	case "gradientlighting":
		d := layerScoreDuration(l)
		if d <= 0 {
			d = 1
		}
		return d, false
	case "colorwavelighting":
		// The captured lighting time is this ColorWave performer's phrase period.
		// Speed remains the propagation control inside the phrase.
		d := layerScoreDuration(l)
		if d <= 0 {
			d = 1
		}
		return d, false
	case "ripplelighting":
		d := layerScoreDuration(l)
		if d <= 0 {
			d = waveTrackRepeat(l, topology, fanOrdinal, ledCount)
		}
		return d, false
	case "wavelighting":
		return waveTrackRepeat(l, topology, fanOrdinal, ledCount), false
	case "rainlighting":
		// Rain is an event generator, not a duration sweep. Use a deterministic
		// event phrase long enough to retain the validated v12.4.38 cadence/fade;
		// serialized duration does not control this repeat.
		speed := number(l.Parameters["speed"], 1)
		if speed <= 0 {
			speed = 1
		}
		return 16 * (0.115 / speed), false
	case "blinklighting", "breathelighting":
		d := layerScoreDuration(l)
		if d <= 0 {
			d = 1
		}
		return d, false
	case "colorshiftlighting", "colorpulselighting", "rainbowlighting", "rainbowwavelighting", "rainbowchannellighting":
		speed := discreteSpeed(l)
		return capturedPeriod(l, 4.0/math.Pow(2, speed-1)), false
	case "spiralrainbowlighting":
		speed := number(l.Parameters["speed"], 2)
		if speed <= 0 {
			speed = 2
		}
		return capturedPeriod(l, 2.0*math.Pow(2, 2-speed)), false
	case "watercolorlighting", "watercolorspectrumlighting":
		d := layerScoreDuration(l)
		if d <= 0 {
			d = 2
		}
		return d, false
	default:
		d := layerScoreDuration(l)
		if d <= 0 {
			d = 1
		}
		return d, false
	}
}

func appendScoreNote(dst *[]LEDCommand, l Layer, li, led int, at float64, col RGB, strength float64) {
	if at < -0.0000005 {
		return
	}
	if strength < 0 {
		strength = 0
	}
	if strength > 1 {
		strength = 1
	}
	// One-shot Static notes retain their authored opacity. The opening score is
	// applied bottom->top at t=0, so opacity reveals the already-established
	// lower opening state instead of pre-multiplying the colour against black.
	*dst = append(*dst, LEDCommand{Time: at, LED: led, Color: col, Strength: strength, LayerIndex: li, Effect: l.Effect})
}

func recordLayerTrack(l Layer, li, fanOrdinal, ledCount int, duration float64, topology LEDTopology, sourceDeviceCount int) ([]LEDCommand, bool) {
	spatial := topologyPositions(topology, fanOrdinal, ledCount)
	notes := make([]LEDCommand, 0)
	effect := strings.ToLower(l.Effect)

	// Static is the conductor's opening tap: establish initial physical state once.
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

	// Every non-static lighting effect is a live performer in v12.4.63. The
	// layer keeps only its authored parameters and independent phrase period;
	// it is evaluated directly from the shared absolute concert clock. There is
	// no pre-recorded 20 ms command grid and no finite master score.
	//
	// Validate support once at t=0. RippleLighting is handled by renderLayer's
	// experimental guide-derived performer below.
	_, _, ok := renderLayer(l, fanOrdinal, ledCount, 0, spatial)
	if !ok {
		return nil, false
	}
	return nil, true
}

func logScoreTimingCandidate(li int, l Layer, topology LEDTopology, fanOrdinal, ledCount int, repeat float64, oneShot bool) {
	effect := strings.ToLower(l.Effect)
	serialized := layerScoreDuration(l)
	logBase := func(model, timing string) {
		log.Printf("iCUE TRACK EFFECT: leds=%d xml=%d effect=%s repeat=%.6fs oneShot=%t serializedDuration=%.6fs model=%s timing=%s", ledCount, li, l.Effect, repeat, oneShot, serialized, model, timing)
	}
	switch effect {
	case "staticlighting":
		logBase("conductor-opening-state", "profile-start-only; never-repeats-with-other-tracks; holds-until-overwritten")
	case "gradientlighting":
		stops := transitionStops(l)
		log.Printf("iCUE TRACK TIMING: leds=%d xml=%d effect=%s stops=%d phrase=%.6fs interpolation=smooth evaluation=LIVE model=guide-gradient", ledCount, li, l.Effect, len(stops), repeat)
		logBase("guide-gradient", "lighting-time=phrase-axis; nodes=control-points; smooth-live-interpolation; no-pre-recorded-grid")
	case "rainlighting":
		speed := number(l.Parameters["speed"], 1)
		if speed <= 0 {
			speed = 1
		}
		log.Printf("iCUE TRACK TIMING: leds=%d xml=%d effect=%s speed=%.3f spawnInterval=%.6fs eventLifetime=%.6fs phrase=%.6fs model=rain-impact-dispersal-v12.4.73", ledCount, li, l.Effect, speed, 0.200/speed, (0.55*(0.200/0.115))/speed, repeat)
		logBase("rain-impact-dispersal-v12.4.73", "same-density-speed-cycle; colour1=impact; colour2=dispersal; transient-restore; serialized-duration-not-repeat")
	case "colorwavelighting":
		speed := number(l.Parameters["speed"], 2)
		if speed < 1 {
			speed = 1
		}
		if speed > 3 {
			speed = 3
		}
		step := 0.200 / speed
		period := float64(maxInt(ledCount, 1))
		if len(topologyPositions(topology, fanOrdinal, ledCount)) == ledCount {
			period = 17
		}
		launch := serialized
		if launch <= 0 {
			launch = 1
		}
		log.Printf("iCUE LIVE PHRASE: leds=%d xml=%d effect=%s speed=%.3f travelStep=%.6fs traversal=%.6fs phrase=%.6fs model=guide-speed-live-phrase", ledCount, li, l.Effect, speed, step, step*period, repeat)
		_ = launch
		logBase("guide-speed-live-phrase", "lighting-time=independent-repeat-period; speed=propagation; no-master-loop")
	case "wavelighting":
		velocity := number(l.Parameters["velocity"], 100)
		if velocity == 0 {
			velocity = 100
		}
		tail := number(l.Parameters["tailLength"], 4)
		if tail <= 0 {
			tail = 1
		}
		log.Printf("iCUE LIVE PHRASE: leds=%d xml=%d effect=%s velocity=%.3f tail=%.3f phrase=%.6fs serializedDuration=%.6fs model=guide-velocity-tail-wave", ledCount, li, l.Effect, velocity, tail, repeat, serialized)
		logBase("guide-velocity-tail-wave", "lighting-time=independent-repeat-period; velocity=travel-speed; tail=wave-tail; no-master-loop")
	case "ripplelighting":
		velocity := number(l.Parameters["velocity"], 100)
		if velocity == 0 {
			velocity = 100
		}
		tail := number(l.Parameters["tailLength"], 4)
		if tail <= 0 {
			tail = 1
		}
		log.Printf("iCUE LIVE PHRASE: leds=%d xml=%d effect=%s velocity=%.3f tail=%.3f phrase=%.6fs model=guide-ripple-probe", ledCount, li, l.Effect, velocity, tail, repeat)
		logBase("guide-ripple-probe", "lighting-time=independent-repeat-period; velocity=ripple-travel; tail=ripple-tail; EXPERIMENTAL")
	default:
		logBase("independent-track", "live-performer; phrase-repeats-independently; no-pre-recorded-grid; no-master-clock")
	}
}

func auditRGBHex(leds []RGB) []string {
	out := make([]string, len(leds))
	for i, c := range leds {
		out[i] = fmt.Sprintf("%d=#%02X%02X%02X", i, int(clamp(c.R)), int(clamp(c.G)), int(clamp(c.B)))
	}
	return out
}

// auditStaticOpeningFrame reconstructs only the authored StaticLighting opening
// state. It deliberately excludes Rain and every other live performer so the
// diagnostic proves what colour each physical LED should hold before live
// effects begin touching it.
func auditStaticOpeningFrame(layers []Layer, topology LEDTopology, deviceOrdinal, ledCount, sourceDeviceCount int) []RGB {
	out := make([]RGB, ledCount)
	for li := len(layers) - 1; li >= 0; li-- {
		l := layers[li]
		if !l.Enabled || !strings.EqualFold(l.Effect, "StaticLighting") {
			continue
		}
		colors, alpha, ok := renderLayer(l, deviceOrdinal, ledCount, 0, topologyPositions(topology, deviceOrdinal, ledCount))
		if !ok {
			continue
		}
		for local := 0; local < ledCount; local++ {
			if !selectedWithTopology(l, topology, deviceOrdinal, local, sourceDeviceCount) || local >= len(colors) || local >= len(alpha) {
				continue
			}
			out[local] = mixRGB(out[local], colors[local], alpha[local])
		}
	}
	return out
}

func auditCanvasStaticOpeningFrame(layers []Layer, logicalBase, ledCount, ordinal int) []RGB {
	out := make([]RGB, ledCount)
	for li := len(layers) - 1; li >= 0; li-- {
		l := layers[li]
		if !l.Enabled || !strings.EqualFold(l.Effect, "StaticLighting") {
			continue
		}
		patches, ok := renderLayerPatches(l, ordinal, ledCount, 0, nil)
		if !ok {
			continue
		}
		for local := 0; local < ledCount; local++ {
			if !selectedAt(l, logicalBase, local) {
				continue
			}
			out[local] = applyPatch(out[local], patches[local])
		}
	}
	return out
}

func compileLiveConcert(p *Profile, topology LEDTopology, deviceOrdinal, ledCount int) *compiledScore {
	log.Printf("iCUE LIVE CONCERT BUILD: v12.4.74g cyclic-spatial-distance-fix leds=%d rendererOrdinal=%d", ledCount, deviceOrdinal)
	d := p.LinkDevice()
	if d == nil || len(d.Channels) == 0 {
		return &compiledScore{unsupported: []string{"LINK device entry missing"}}
	}
	layers := p.expandLightingLinks(d.Channels[0].Layers)
	sourceDeviceCount := sourceQXFanCount(layers, topology)
	if topology != nil {
		logical := make([]int, 0, ledCount)
		for local := 0; local < ledCount; local++ {
			if id, ok := topology.LogicalLED(deviceOrdinal, local); ok {
				logical = append(logical, id)
			} else {
				logical = append(logical, -1)
			}
		}
		log.Printf("iCUE LED TOPOLOGY AUDIT: topology=%s rendererOrdinal=%d leds=%d localToLogical=%v", topology.Name(), deviceOrdinal, ledCount, logical)
		staticFrame := auditStaticOpeningFrame(layers, topology, deviceOrdinal, ledCount, sourceDeviceCount)
		log.Printf("iCUE STATIC FRAME AUDIT: topology=%s rendererOrdinal=%d leds=%d preLiveRGB=%v", topology.Name(), deviceOrdinal, ledCount, auditRGBHex(staticFrame))
	}
	// v12.4.74b diagnostic: prove whether a ConcreteLeds Static mask is
	// actually identical across QX ordinals before changing any mapping.
	// This is intentionally observational only: it does not alter selection.
	if topology != nil && strings.EqualFold(topology.Name(), "QX") {
		for li, l := range layers {
			if !l.Enabled || !strings.EqualFold(l.Effect, "StaticLighting") || !strings.EqualFold(l.SelectionMode, "ConcreteLeds") {
				continue
			}
			selected := make([]int, 0, ledCount)
			logicalIDs := make([]int, 0, ledCount)
			for local := 0; local < ledCount; local++ {
				if !selectedWithTopology(l, topology, deviceOrdinal, local, sourceDeviceCount) {
					continue
				}
				selected = append(selected, local)
				mappedOrdinal := deviceOrdinal
				if sourceDeviceCount > 0 && deviceOrdinal >= sourceDeviceCount {
					mappedOrdinal = deviceOrdinal % sourceDeviceCount
				}
				if logical, ok := topology.LogicalLED(mappedOrdinal, local); ok {
					logicalIDs = append(logicalIDs, logical)
				}
			}
			log.Printf("iCUE CONCRETE MAP AUDIT: device=%d xml=%d effect=%s sourceQXFans=%d authoredRefs=%d selectedCount=%d localLEDs=%v logicalLEDs=%v", deviceOrdinal, li, l.Effect, sourceDeviceCount, len(l.LEDs), len(selected), selected, logicalIDs)
		}
	}
	score := &compiledScore{}
	unsupported := map[string]bool{}
	for li, l := range layers {
		if !l.Enabled {
			continue
		}
		repeat, oneShot := trackRepeatDuration(l, topology, deviceOrdinal, ledCount)
		recordDuration := repeat
		if oneShot {
			recordDuration = 0
		}
		trackNotes, ok := recordLayerTrack(l, li, deviceOrdinal, ledCount, recordDuration, topology, sourceDeviceCount)
		if !ok {
			unsupported[l.Effect] = true
			continue
		}
		sort.SliceStable(trackNotes, func(i, j int) bool {
			if math.Abs(trackNotes[i].Time-trackNotes[j].Time) > 0.0000005 {
				return trackNotes[i].Time < trackNotes[j].Time
			}
			if trackNotes[i].LED != trackNotes[j].LED {
				return trackNotes[i].LED < trackNotes[j].LED
			}
			return trackNotes[i].LayerIndex > trackNotes[j].LayerIndex
		})
		score.tracks = append(score.tracks, compiledTrack{layerIndex: li, effect: l.Effect, repeat: repeat, oneShot: oneShot, live: !oneShot, layer: l, notes: trackNotes})
		score.totalNotes += len(trackNotes)
		log.Printf("iCUE TRACK COMPILED: leds=%d xml=%d effect=%s notes=%d repeat=%.6fs oneShot=%t live=%t", ledCount, li, l.Effect, len(trackNotes), repeat, oneShot, !oneShot)
		logScoreTimingCandidate(li, l, topology, deviceOrdinal, ledCount, repeat, oneShot)
	}
	for name := range unsupported {
		score.unsupported = append(score.unsupported, name)
	}
	sort.Strings(score.unsupported)
	log.Printf("iCUE LIVE CONCERT READY: leds=%d rendererOrdinal=%d performers=%d storedDiscreteNotes=%d concertEnd=NEVER sharedClock=absolute allPerformers=LIVE propagationClock=ABSOLUTE phraseClock=INDEPENDENT", ledCount, deviceOrdinal, len(score.tracks), score.totalNotes)
	return score
}

type absoluteScoreNote struct {
	at   float64
	note LEDCommand
}

func collectTrackRange(track compiledTrack, from, to float64, includeStart bool, dst *[]absoluteScoreNote) int {
	if to < 0 || to+0.0000005 < from {
		return 0
	}
	count := 0
	if track.oneShot {
		if includeStart && from <= 0.0000005 && to >= -0.0000005 {
			for _, n := range track.notes {
				*dst = append(*dst, absoluteScoreNote{at: n.Time, note: n})
				count++
			}
		}
		return count
	}
	if track.repeat <= 0 || len(track.notes) == 0 {
		return 0
	}
	firstCycle := int64(math.Floor(from / track.repeat))
	if firstCycle < 0 {
		firstCycle = 0
	}
	lastCycle := int64(math.Floor(to / track.repeat))
	for cycle := firstCycle; cycle <= lastCycle; cycle++ {
		base := float64(cycle) * track.repeat
		for _, n := range track.notes {
			at := base + n.Time
			if at > to+0.0000005 {
				break
			}
			eligible := at > from+0.0000005
			if includeStart && at >= from-0.0000005 {
				eligible = true
			}
			if !eligible {
				continue
			}
			*dst = append(*dst, absoluteScoreNote{at: at, note: n})
			count++
		}
	}
	return count
}

func visualDistance(a, b RGB) float64 {
	dr, dg, db := a.R-b.R, a.G-b.G, a.B-b.B
	return math.Sqrt(dr*dr + dg*dg + db*db)
}

func applyLiveContinuous(state *scorePlayerState, at float64) int {
	changed := map[int]struct{}{}
	// v12.4.64: every performer receives the absolute concert clock. A layer's
	// authored phrase may repeat its expression/colour programme, but reaching a
	// phrase boundary must not rewind propagation that is already travelling.
	// renderLayer owns the effect-specific distinction: Gradient/Breathe/etc.
	// repeat their expression mathematically, while Wave/ColorWave/Rain/Ripple
	// keep continuous propagation/event time across those boundaries.
	for ti := len(state.score.tracks) - 1; ti >= 0; ti-- {
		track := state.score.tracks[ti]
		if !track.live || track.repeat <= 0 {
			continue
		}
		spatial := topologyPositions(state.topology, state.deviceOrdinal, len(state.leds))
		colors, alpha, ok := renderLayer(track.layer, state.deviceOrdinal, len(state.leds), at, spatial)
		if !ok {
			continue
		}
		var sum RGB
		active := 0
		firstActive, lastActive := -1, -1
		for led := 0; led < len(state.leds); led++ {
			if !selectedWithTopology(track.layer, state.topology, state.deviceOrdinal, led, state.sourceDeviceCount) {
				continue
			}
			if led >= len(colors) || led >= len(alpha) || alpha[led] <= 0.000001 {
				// Rain is a transient command source. When its event ends, restore the
				// colour that was physically present when the drop began, but only if
				// nobody else has changed the LED since Rain's last command. This is
				// NO COMMAND semantics with an interruptible transient restore, not
				// layer ownership.
				if strings.EqualFold(track.effect, "RainLighting") && ti < len(state.rainActive) && state.rainActive[ti][led] {
					last := state.rainLast[ti][led]
					cur := state.leds[led]
					if visualDistance(cur, last) < 0.5 {
						state.leds[led] = state.rainBase[ti][led]
						changed[led] = struct{}{}
					}
					state.rainActive[ti][led] = false
				}
				continue // NO COMMAND: retain current physical LED state.
			}
			var cmd RGB
			if strings.EqualFold(track.effect, "RainLighting") && ti < len(state.rainActive) {
				if !state.rainActive[ti][led] {
					state.rainBase[ti][led] = state.leds[led]
					state.rainActive[ti][led] = true
				} else if visualDistance(state.leds[led], state.rainLast[ti][led]) >= 0.5 {
					// Another performer changed the physical LED while this Rain transient
					// was active. Cancel the stale restore/base and let this Rain command
					// start from the newly committed physical colour. No layer owns an LED.
					state.rainBase[ti][led] = state.leds[led]
				}
				// Rain intensity is a transient overlay over the colour that existed
				// before the drop, not a fade toward black.
				base := state.rainBase[ti][led]
				strength := alpha[led]
				if strength > 1 {
					strength = 1
				}
				cmd = mixRGB(base, colors[led], strength)
				state.rainLast[ti][led] = cmd
			} else {
				cmd = concreteCommand(colors[led], alpha[led])
			}
			if firstActive < 0 {
				firstActive = led
			}
			lastActive = led
			state.leds[led] = cmd
			changed[led] = struct{}{}
			sum.R += cmd.R
			sum.G += cmd.G
			sum.B += cmd.B
			active++
		}

		// Diagnostic visual cue: report only a large representative colour change,
		// so a real-hardware observer can compare the timestamp with what iCUE/OLH
		// visibly did. This does not affect rendering and is deliberately limited
		// to rendererOrdinal 0 to avoid duplicate fan/device spam.
		if state.deviceOrdinal == 0 && active > 0 && ti < len(state.visualSamples) {
			rep := RGB{R: sum.R / float64(active), G: sum.G / float64(active), B: sum.B / float64(active)}
			if state.visualSampleSet[ti] {
				delta := visualDistance(state.visualSamples[ti], rep)
				if delta >= 80 && at-state.visualLogAt[ti] >= 0.20 {
					local := math.Mod(at, track.repeat)
					if local < 0 {
						local += track.repeat
					}
					prev := state.visualSamples[ti]
					expect := "noticeable-colour-change"
					switch strings.ToLower(track.effect) {
					case "wavelighting", "colorwavelighting":
						expect = "travelling-blended-colour-change"
					case "gradientlighting", "breathelighting", "colorshiftlighting", "colorpulselighting", "watercolorlighting":
						expect = "gradual-expression-change"
					case "rainlighting", "ripplelighting":
						expect = "event-hit-or-propagation-change"
					}
					log.Printf("iCUE VISUAL CUE: t=%.3fs xml=%d effect=%s phraseLocal=%.3fs activeLEDs=%d region=LED%d-LED%d avgRGB=(%.0f,%.0f,%.0f)->(%.0f,%.0f,%.0f) delta=%.1f EXPECT=%s", at, track.layerIndex, track.effect, local, active, firstActive+1, lastActive+1, prev.R, prev.G, prev.B, rep.R, rep.G, rep.B, delta, expect)
					state.visualLogAt[ti] = at
				}
			}
			state.visualSamples[ti] = rep
			state.visualSampleSet[ti] = true
		}
	}
	return len(changed)
}

func applyAbsoluteRange(state *scorePlayerState, from, to float64, includeStart bool) (int, int) {
	events := make([]absoluteScoreNote, 0)
	for _, track := range state.score.tracks {
		collectTrackRange(track, from, to, includeStart, &events)
	}
	// Chronological latest command wins. Only genuinely identical timestamps on
	// the same LED use XML/UI priority, with xml0/topmost applied last.
	sort.SliceStable(events, func(i, j int) bool {
		a, b := events[i], events[j]
		if math.Abs(a.at-b.at) > 0.0000005 {
			return a.at < b.at
		}
		if a.note.LED != b.note.LED {
			return a.note.LED < b.note.LED
		}
		return a.note.LayerIndex > b.note.LayerIndex
	})
	changed := map[int]struct{}{}
	consumed := 0
	for _, ev := range events {
		n := ev.note
		if n.LED < 0 || n.LED >= len(state.leds) {
			continue
		}
		if strings.EqualFold(n.Effect, "StaticLighting") {
			// Static is an opening command only. Its sole authored modifier is
			// opacity, which composites this opening command over the physical state
			// established by lower Static layers at the same t=0 timestamp.
			state.leds[n.LED] = mixRGB(state.leds[n.LED], n.Color, n.Strength)
		} else {
			state.leds[n.LED] = n.Color
		}
		consumed++
		changed[n.LED] = struct{}{}
	}
	liveChanged := applyLiveContinuous(state, to)
	if liveChanged > len(changed) {
		return consumed, liveChanged
	}
	return consumed, len(changed)
}

// RenderFan advances the live concert. Every performer repeats only its own phrase
// on one absolute clock. The concert itself has no end or master modulo/reset.
func (p *Profile) RenderFan(topology LEDTopology, deviceOrdinal, ledCount int, started time.Time, brightness float64) ([]byte, []string) {
	key := persistentKey(deviceOrdinal, ledCount, started)
	scorePlayers.Lock()
	state := scorePlayers.byRun[key]
	if state == nil {
		score := compileLiveConcert(p, topology, deviceOrdinal, ledCount)
		d := p.LinkDevice()
		sourceCount := 0
		if d != nil && len(d.Channels) > 0 {
			sourceCount = sourceQXFanCount(p.expandLightingLinks(d.Channels[0].Layers), topology)
		}
		state = &scorePlayerState{score: score, topology: topology, deviceOrdinal: deviceOrdinal, sourceDeviceCount: sourceCount, leds: make([]RGB, ledCount), visualSamples: make([]RGB, len(score.tracks)), visualSampleSet: make([]bool, len(score.tracks)), visualLogAt: make([]float64, len(score.tracks)), rainBase: make([][]RGB, len(score.tracks)), rainLast: make([][]RGB, len(score.tracks)), rainActive: make([][]bool, len(score.tracks))}
		for ti := range score.tracks {
			state.rainBase[ti] = make([]RGB, ledCount)
			state.rainLast[ti] = make([]RGB, ledCount)
			state.rainActive[ti] = make([]bool, ledCount)
		}
		scorePlayers.byRun[key] = state
		if topology != nil && strings.EqualFold(topology.Name(), "QX") {
			log.Printf("iCUE LIVE STATE AUDIT: v12.4.74f topology=%s rendererOrdinal=%d stateDeviceOrdinal=%d leds=%d keyStarted=%d keyDeviceOrdinal=%d keyLedCount=%d state=%p", topology.Name(), deviceOrdinal, state.deviceOrdinal, ledCount, key.started, key.deviceOrdinal, key.ledCount, state)
		}
	}
	elapsed := time.Since(started).Seconds()
	from := state.lastElapsed
	consumed, changed := 0, 0
	if !state.started {
		consumed, changed = applyAbsoluteRange(state, 0, elapsed, true)
		state.started = true
	} else if elapsed+0.0000005 < state.lastElapsed {
		for i := range state.leds {
			state.leds[i] = RGB{}
		}
		consumed, changed = applyAbsoluteRange(state, 0, elapsed, true)
		from = 0
	} else {
		consumed, changed = applyAbsoluteRange(state, state.lastElapsed, elapsed, false)
	}
	// Measure the actual RenderFan invocation cadence independently of the
	// once-per-second playback diagnostic. This tells us whether the renderer is
	// really being called near the hardware output cadence or whether upstream
	// scheduling is starving it.
	if state.started && from > 0 && elapsed >= from {
		delta := elapsed - from
		if delta > 0 {
			state.renderCalls++
			state.renderDeltaSum += delta
			if state.renderDeltaMin == 0 || delta < state.renderDeltaMin {
				state.renderDeltaMin = delta
			}
			if delta > state.renderDeltaMax {
				state.renderDeltaMax = delta
			}
		}
	}
	if deviceOrdinal == 0 && elapsed-state.renderCadenceAt >= 1.0 && state.renderCalls > 0 {
		window := elapsed - state.renderCadenceAt
		avg := state.renderDeltaSum / float64(state.renderCalls)
		fps := 0.0
		if window > 0 {
			fps = float64(state.renderCalls) / window
		}
		log.Printf("iCUE RENDER CADENCE: device=%d leds=%d window=%.3fs calls=%d callsPerSecond=%.1f deltaAvg=%.3fms deltaMin=%.3fms deltaMax=%.3fms", deviceOrdinal, ledCount, window, state.renderCalls, fps, avg*1000, state.renderDeltaMin*1000, state.renderDeltaMax*1000)
		state.renderCalls = 0
		state.renderDeltaSum = 0
		state.renderDeltaMin = 0
		state.renderDeltaMax = 0
		state.renderCadenceAt = elapsed
	}
	// v12.4.74f diagnostic: follow the ten authored orange QX LEDs (local 24-33)
	// through live playback. This is observational only. It proves whether the
	// renderer ordinal/42-ID block survives into the live state and whether Rain
	// restores each orange LED to the colour it had before the transient.
	if state.topology != nil && strings.EqualFold(state.topology.Name(), "QX") && ledCount >= 34 && elapsed-state.orangeAuditAt >= 1.0 {
		entries := make([]string, 0, 10)
		for led := 24; led <= 33 && led < len(state.leds); led++ {
			logical := -1
			if id, ok := state.topology.LogicalLED(state.deviceOrdinal, led); ok {
				logical = id
			}
			rainActive := false
			rainBase := RGB{}
			rainLast := RGB{}
			for ti, track := range state.score.tracks {
				if !strings.EqualFold(track.effect, "RainLighting") || ti >= len(state.rainActive) || led >= len(state.rainActive[ti]) {
					continue
				}
				if state.rainActive[ti][led] {
					rainActive = true
					rainBase = state.rainBase[ti][led]
					rainLast = state.rainLast[ti][led]
				}
			}
			entries = append(entries, fmt.Sprintf("L%d->ID%d cur=#%02X%02X%02X rain=%t base=#%02X%02X%02X last=#%02X%02X%02X", led, logical, byte(clamp(state.leds[led].R)), byte(clamp(state.leds[led].G)), byte(clamp(state.leds[led].B)), rainActive, byte(clamp(rainBase.R)), byte(clamp(rainBase.G)), byte(clamp(rainBase.B)), byte(clamp(rainLast.R)), byte(clamp(rainLast.G)), byte(clamp(rainLast.B))))
		}
		log.Printf("iCUE ORANGE LIVE AUDIT: v12.4.74f rendererOrdinal=%d stateDeviceOrdinal=%d keyDeviceOrdinal=%d leds=%d orange=[%s]", deviceOrdinal, state.deviceOrdinal, key.deviceOrdinal, ledCount, strings.Join(entries, " | "))
		state.orangeAuditAt = elapsed
	}
	state.lastElapsed = elapsed
	bucket := int64(math.Floor(elapsed))
	if deviceOrdinal == 0 && bucket != state.logBucket {
		log.Printf("iCUE LIVE PLAYBACK: device=%d leds=%d concertTime=%.6f from=%.6f concertEnd=NEVER notesConsumed=%d ledTouches=%d performers=%d storedDiscreteNotes=%d", deviceOrdinal, ledCount, elapsed, from, consumed, changed, len(state.score.tracks), state.score.totalNotes)
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
		// iCUE Rain speed changes how quickly the same-density rain pattern cycles,
		// rather than changing how much rain exists at once. Scale both the spawn
		// cadence and event lifetime by the same factor so their overlap ratio stays
		// constant at Slow/Normal/Fast. The temporary calibration controls supply the cadence for each preset.
		stepDuration := rainStepDuration(speed)
		// Preserve the calibrated overlap/density ratio while the timing slider moves.
		dropDuration := stepDuration * (0.55 / 0.115)
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
			bestNorm := 0.0
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
				// coord may be a global topology coordinate spanning multiple physical
				// devices (QX Position deliberately adds the device pitch), while target
				// lives inside this effect's compact repeating period. Wrap the delta
				// before measuring circular distance. The previous min(d, period-d)
				// becomes negative once d > period, which made every Rain LED on QX1+
				// look active at once.
				delta := wrapPeriodic(coord-target, period)
				d := math.Min(delta, period-delta)
				if d > 0.55 {
					continue
				}
				norm := age / dropDuration
				intensity := math.Pow(1-norm, 1.9)
				if intensity > best {
					best = intensity
					bestNorm = norm
				}
			}
			if best <= 0 {
				a[i] = 0
				continue
			}
			// iCUE Rain uses its two authored colours as phases of one drop, not
			// as a random per-drop colour choice: colour 1 is the initial impact,
			// then colour 2 is the dispersal/tail. Keep a short impact hold and a
			// smooth hand-off so the transition is visible without introducing a
			// hard colour flash. The transient alpha still controls the dispersal
			// strength and the player restores the interrupted physical colour when
			// the event finishes (unless a newer performer superseded it).
			phaseMix := 0.0
			if bestNorm > 0.25 {
				phaseMix = (bestNorm - 0.25) / 0.15
				if phaseMix > 1 {
					phaseMix = 1
				}
				phaseMix = phaseMix * phaseMix * (3 - 2*phaseMix)
			}
			c[i] = mixRGB(c1, c2, phaseMix)
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
		// continuous route clock. RX/iCUE timing calibration puts the reference speed=2 at about
		// 100 ms per route position (roughly 1.7 s across the 17-position LINK
		// route). Slow/Normal/Fast retain the authored 1/2/3 relationship; only
		// the ColorWave base propagation rate is calibrated here.
		lifetime := number(l.Parameters["duration"], 1000) / 1000
		if lifetime <= 0 {
			lifetime = 1
		}
		stepSeconds := 0.2 / speed
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
				routeCoord = wrapPeriodic(period-coord, period)
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
	case "ripplelighting":
		// v12.4.63a experimental live Ripple probe. The historical iCUE guide
		// separates Ripple Velocity and Tail. Treat this layer's elapsed value as
		// its independent phrase clock; expand from route origin on the 1-D LINK
		// topology and use the authored transition curve to shape the tail.
		velocity := math.Abs(number(l.Parameters["velocity"], 100))
		if velocity <= 0.000001 {
			velocity = 100
		}
		tail := number(l.Parameters["tailLength"], 4)
		if tail <= 0 {
			tail = 1
		}
		stops := transitionStops(l)
		if len(stops) == 0 {
			col := layerColor(l, RGB{R: 255, G: 255, B: 255})
			stops = []transitionStop{{0, col, 0}, {1, col, 1}}
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
		radius := math.Mod(elapsed*velocity, span+tail)
		for i, x := range coords {
			dist := math.Abs(x - minC)
			d := radius - dist
			if d < 0 || d > tail {
				a[i] = 0
				continue
			}
			t := d / tail
			col, alpha := sampleTransitionStops(stops, t)
			c[i] = col
			a[i] *= alpha
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

// EditorColor is the canonical colour representation exposed by the iCUE
// workspace API. The browser deliberately does not decode Corsair colour
// serialization itself; these values come from the same decoder used by the
// renderer.
type EditorColor struct {
	Hex string  `json:"hex"`
	R   float64 `json:"r"`
	G   float64 `json:"g"`
	B   float64 `json:"b"`
}

// EditorLayer is the normalized, read-only view of an authored layer. RawParameters
// is retained verbatim so reverse-engineering never loses sight of what iCUE
// actually serialized, while Colors is renderer-decoded data suitable for UI use.
type EditorLayer struct {
	Index         int            `json:"index"`
	Name          string         `json:"name"`
	Effect        string         `json:"effect"`
	EffectName    string         `json:"effectName,omitempty"`
	Enabled       bool           `json:"enabled"`
	SelectionMode string         `json:"selectionMode,omitempty"`
	LEDs          []string       `json:"leds,omitempty"`
	Colors        []EditorColor  `json:"colors"`
	PrimaryColor  *EditorColor   `json:"primaryColor,omitempty"`
	RawParameters map[string]any `json:"rawParameters,omitempty"`
}

func editorColor(c RGB) EditorColor {
	clamp := func(v float64) int {
		if v < 0 {
			return 0
		}
		if v > 255 {
			return 255
		}
		return int(math.Round(v))
	}
	r, g, b := clamp(c.R), clamp(c.G), clamp(c.B)
	return EditorColor{Hex: fmt.Sprintf("#%02x%02x%02x", r, g, b), R: c.R, G: c.G, B: c.B}
}

func decodedLayerColors(l Layer) []EditorColor {
	// Decode only fields that are known to carry authored colour data.  The
	// parser intentionally preserves cereal collections in several shapes
	// ([]string, []any and nested maps), so walk those values recursively rather
	// than depending on one concrete Go type.
	var rgb []RGB
	for _, key := range []string{"transitions", "colors", "color"} {
		if v, ok := l.Parameters[key]; ok {
			collectDecodedColors(v, &rgb)
		}
	}

	out := make([]EditorColor, 0, len(rgb))
	seen := map[string]bool{}
	for _, c := range rgb {
		e := editorColor(c)
		if !seen[e.Hex] {
			seen[e.Hex] = true
			out = append(out, e)
		}
	}
	return out
}

// collectDecodedColors accepts the generic shapes produced by the cueprofile
// parser without teaching the browser anything about Corsair serialization.
// Only values below known colour-bearing parameter keys are passed here, which
// prevents ordinary numeric effect parameters from being mistaken for colours.
func collectDecodedColors(v any, out *[]RGB) {
	switch x := v.(type) {
	case []string:
		for _, item := range x {
			collectDecodedColors(item, out)
		}
	case []any:
		for _, item := range x {
			collectDecodedColors(item, out)
		}
	case map[string]any:
		// Prefer explicit colour members when a structured object survives the
		// parser, otherwise recurse so future parser enrichment remains compatible.
		for _, key := range []string{"color", "colour", "value", "first", "second"} {
			if item, ok := x[key]; ok {
				collectDecodedColors(item, out)
			}
		}
		for key, item := range x {
			switch strings.ToLower(key) {
			case "color", "colour", "value", "first", "second":
				continue
			}
			collectDecodedColors(item, out)
		}
	default:
		if c, ok := parseColor(v); ok {
			*out = append(*out, c)
		}
	}
}

// DecodeEditorLayers exposes renderer-decoded authored values without changing
// the parsed Layer objects used for playback. This is the canonical API bridge
// between cueprofile parsing, rendering and the profile editor.
func DecodeEditorLayers(layers []Layer) []EditorLayer {
	out := make([]EditorLayer, 0, len(layers))
	for i, l := range layers {
		colors := decodedLayerColors(l)
		e := EditorLayer{Index: i, Name: l.Name, Effect: l.Effect, EffectName: l.EffectName, Enabled: l.Enabled, SelectionMode: l.SelectionMode, LEDs: append([]string(nil), l.LEDs...), Colors: colors, RawParameters: l.Parameters}
		if len(colors) > 0 {
			c := colors[0]
			e.PrimaryColor = &c
		}
		out = append(out, e)
	}
	return out
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

// wrapPeriodic maps a global topology coordinate/delta into one positive
// effect-local period. Spatial topologies may span multiple physical devices,
// while periodic effects such as Rain and ColorWave repeat on a compact route.
// Go's math.Mod preserves the sign of a negative dividend, so using it directly
// for reverse-direction coordinates can leak negative route positions into
// event/pass calculations on later devices.
func wrapPeriodic(x, period float64) float64 {
	if period <= 0 {
		return 0
	}
	x = math.Mod(x, period)
	if x < 0 {
		x += period
	}
	return x
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

// RuntimeLayerEdit is a non-persistent live editor patch. It deliberately
// changes only the cached parsed profile in memory; the source .cueprofile is
// never rewritten. Re-uploading the file or restarting OpenLinkHub restores
// the authored iCUE values.
type RuntimeLayerEdit struct {
	Action        string         `json:"action,omitempty"`
	DeviceIndex   int            `json:"deviceIndex"`
	ChannelIndex  int            `json:"channelIndex"`
	LayerIndex    int            `json:"layerIndex"`
	LightingLink  bool           `json:"lightingLink"`
	Name          *string        `json:"name,omitempty"`
	Effect        *string        `json:"effect,omitempty"`
	Enabled       *bool          `json:"enabled,omitempty"`
	SelectionMode *string        `json:"selectionMode,omitempty"`
	LEDs          *[]string      `json:"leds,omitempty"`
	Parameters    map[string]any `json:"parameters,omitempty"`
}

func cloneMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// ApplyRuntimeLayerEdit applies a temporary layer patch and drops compiled live
// concerts so the next hardware frame recompiles from the edited values. The
// scorePlayers lock is also the RenderFan compile lock, which prevents a live
// renderer from reading the layer while it is being changed.
func ApplyRuntimeLayerEdit(p *Profile, edit RuntimeLayerEdit) (Layer, error) {
	if p == nil {
		return Layer{}, fmt.Errorf("profile is nil")
	}
	scorePlayers.Lock()
	defer scorePlayers.Unlock()

	var layer *Layer
	if edit.LightingLink && strings.TrimSpace(edit.Action) != "" {
		action := strings.ToLower(strings.TrimSpace(edit.Action))
		switch action {
		case "add":
			p.LightingLinkLayers = append(p.LightingLinkLayers, Layer{Name: "New Layer", Enabled: true, SelectionMode: "AllLeds", Effect: "StaticLighting", Parameters: map[string]any{"colors": []any{"rgb(1 1 1)"}}, StartWithProfile: true, StopAfterTimes: -1, PlayOption: "Standard"})
			edit.LayerIndex = len(p.LightingLinkLayers) - 1
		case "duplicate":
			if edit.LayerIndex < 0 || edit.LayerIndex >= len(p.LightingLinkLayers) {
				return Layer{}, fmt.Errorf("lighting-link layer %d is out of range", edit.LayerIndex)
			}
			clone := p.LightingLinkLayers[edit.LayerIndex]
			clone.Name = strings.TrimSpace(clone.Name + " Copy")
			clone.LEDs = append([]string(nil), clone.LEDs...)
			clone.Parameters = cloneMap(clone.Parameters)
			p.LightingLinkLayers = append(p.LightingLinkLayers, Layer{})
			copy(p.LightingLinkLayers[edit.LayerIndex+2:], p.LightingLinkLayers[edit.LayerIndex+1:])
			p.LightingLinkLayers[edit.LayerIndex+1] = clone
			edit.LayerIndex++
		case "delete":
			if edit.LayerIndex < 0 || edit.LayerIndex >= len(p.LightingLinkLayers) {
				return Layer{}, fmt.Errorf("lighting-link layer %d is out of range", edit.LayerIndex)
			}
			p.LightingLinkLayers = append(p.LightingLinkLayers[:edit.LayerIndex], p.LightingLinkLayers[edit.LayerIndex+1:]...)
			if len(p.LightingLinkLayers) == 0 {
				p.LightingLinkLayers = append(p.LightingLinkLayers, Layer{Name: "New Layer", Enabled: true, SelectionMode: "AllLeds", Effect: "StaticLighting", Parameters: map[string]any{"colors": []any{"rgb(1 1 1)"}}, StartWithProfile: true, StopAfterTimes: -1, PlayOption: "Standard"})
			}
			if edit.LayerIndex >= len(p.LightingLinkLayers) {
				edit.LayerIndex = len(p.LightingLinkLayers) - 1
			}
		case "up", "down":
			if edit.LayerIndex < 0 || edit.LayerIndex >= len(p.LightingLinkLayers) {
				return Layer{}, fmt.Errorf("lighting-link layer %d is out of range", edit.LayerIndex)
			}
			other := edit.LayerIndex - 1
			if action == "down" {
				other = edit.LayerIndex + 1
			}
			if other >= 0 && other < len(p.LightingLinkLayers) {
				p.LightingLinkLayers[edit.LayerIndex], p.LightingLinkLayers[other] = p.LightingLinkLayers[other], p.LightingLinkLayers[edit.LayerIndex]
				edit.LayerIndex = other
			}
		default:
			return Layer{}, fmt.Errorf("unknown layer action %q", edit.Action)
		}
		layer = &p.LightingLinkLayers[edit.LayerIndex]
	} else if edit.LightingLink {
		if edit.LayerIndex < 0 || edit.LayerIndex >= len(p.LightingLinkLayers) {
			return Layer{}, fmt.Errorf("lighting-link layer %d is out of range", edit.LayerIndex)
		}
		layer = &p.LightingLinkLayers[edit.LayerIndex]
	} else {
		if edit.DeviceIndex < 0 || edit.DeviceIndex >= len(p.Devices) {
			return Layer{}, fmt.Errorf("device %d is out of range", edit.DeviceIndex)
		}
		dev := &p.Devices[edit.DeviceIndex]
		if edit.ChannelIndex < 0 || edit.ChannelIndex >= len(dev.Channels) {
			return Layer{}, fmt.Errorf("channel %d is out of range", edit.ChannelIndex)
		}
		ch := &dev.Channels[edit.ChannelIndex]
		if edit.LayerIndex < 0 || edit.LayerIndex >= len(ch.Layers) {
			return Layer{}, fmt.Errorf("layer %d is out of range", edit.LayerIndex)
		}
		layer = &ch.Layers[edit.LayerIndex]
	}

	if edit.Name != nil {
		layer.Name = strings.TrimSpace(*edit.Name)
	}
	if edit.Effect != nil && strings.TrimSpace(*edit.Effect) != "" {
		layer.Effect = strings.TrimSpace(*edit.Effect)
	}
	if edit.Enabled != nil {
		layer.Enabled = *edit.Enabled
	}
	if edit.SelectionMode != nil {
		layer.SelectionMode = strings.TrimSpace(*edit.SelectionMode)
	}
	if edit.LEDs != nil {
		layer.LEDs = append([]string(nil), (*edit.LEDs)...)
	}
	if layer.Parameters == nil {
		layer.Parameters = map[string]any{}
	}
	for k, v := range edit.Parameters {
		if strings.TrimSpace(k) != "" {
			layer.Parameters[k] = v
		}
	}

	// A parameter/selection change can alter both discrete notes and continuous
	// performers. Rebuild every current concert rather than attempting to patch
	// compiled state in place. The profile source on disk remains untouched.
	scorePlayers.byRun = make(map[persistentStateKey]*scorePlayerState)
	persistentLEDStates.Lock()
	persistentLEDStates.byRun = make(map[persistentStateKey]*persistentLEDState)
	persistentLEDStates.Unlock()
	return *layer, nil
}
