package icueprofile

import (
	"fmt"
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

func rgbCommandNear(a, b RGB, eps float64) bool {
	return math.Abs(a.R-b.R) <= eps && math.Abs(a.G-b.G) <= eps && math.Abs(a.B-b.B) <= eps
}

var ledNumber = regexp.MustCompile(`(?i)Led_Cmd_Led(\d+)$`)

// v12.4.45 patch resolver: effects generate sparse LED patches against a shared
// profile clock. A patch has an explicit active bit, so "no command" can never
// be confused with RGB black. Layers are applied base-to-top using iCUE's
// serialized top-to-bottom stack order. Authored black remains an active patch.

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

// TimingCalibration is a runtime-only calibration bench for the shared effect
// clock. Normal remains the authored 1.0x reference; Slow and Fast can be
// adjusted live while comparing OpenLinkHub against iCUE. Nothing is written
// back to the imported .cueprofile.
type TimingCalibration struct {
	Slow float64 `json:"slow"`
	Fast float64 `json:"fast"`
}

var timingCalibration = struct {
	sync.RWMutex
	Slow float64
	Fast float64
}{Slow: 1.5, Fast: 0.5}

func GetTimingCalibration() TimingCalibration {
	timingCalibration.RLock()
	defer timingCalibration.RUnlock()
	return TimingCalibration{Slow: timingCalibration.Slow, Fast: timingCalibration.Fast}
}

// RainMethod selects only the Rain renderer used by imported iCUE profiles.
// It is runtime-only so A/B testing never mutates the source .cueprofile.
type RainMethod struct {
	Method string `json:"method"`
}

var rainMethod = struct {
	sync.RWMutex
	Method string
}{Method: "normalized"}

// MappingProbe is a runtime-only LED identification overlay for verified LINK
// topologies. It bypasses profile/effect output only for the selected QX/TITAN
// device: every LED is blue and the selected local LED is purple. The authored profile and live
// concert state continue untouched underneath so disabling the probe resumes
// normal playback immediately.
type MappingProbe struct {
	Enabled       bool   `json:"enabled"`
	Kind          string `json:"kind"`
	DeviceOrdinal int    `json:"deviceOrdinal"`
	LocalLED      int    `json:"localLed"`
}

var mappingProbe = struct {
	sync.RWMutex
	Enabled       bool
	Kind          string
	DeviceOrdinal int
	LocalLED      int
}{}

func GetMappingProbe() MappingProbe {
	mappingProbe.RLock()
	defer mappingProbe.RUnlock()
	return MappingProbe{Enabled: mappingProbe.Enabled, Kind: mappingProbe.Kind, DeviceOrdinal: mappingProbe.DeviceOrdinal, LocalLED: mappingProbe.LocalLED}
}

func SetMappingProbe(enabled bool, kind string, deviceOrdinal, localLED int) (MappingProbe, error) {
	kind = strings.ToLower(strings.TrimSpace(kind))
	maxLED := 0
	switch kind {
	case "qx":
		maxLED = 34
	case "titan":
		maxLED = 20
	default:
		if enabled {
			return GetMappingProbe(), fmt.Errorf("mapping probe supports QX and TITAN only")
		}
	}
	if deviceOrdinal < 0 {
		return GetMappingProbe(), fmt.Errorf("device ordinal must be zero or greater")
	}
	if enabled && (localLED < 0 || localLED >= maxLED) {
		return GetMappingProbe(), fmt.Errorf("%s local LED must be between 0 and %d", strings.ToUpper(kind), maxLED-1)
	}
	mappingProbe.Lock()
	mappingProbe.Enabled = enabled
	mappingProbe.Kind = kind
	mappingProbe.DeviceOrdinal = deviceOrdinal
	mappingProbe.LocalLED = localLED
	mappingProbe.Unlock()
	return GetMappingProbe(), nil
}

func GetRainMethod() RainMethod {
	rainMethod.RLock()
	defer rainMethod.RUnlock()
	return RainMethod{Method: rainMethod.Method}
}

func SetRainMethod(method string) (RainMethod, error) {
	method = strings.ToLower(strings.TrimSpace(method))
	if method != "normalized" && method != "native" {
		return GetRainMethod(), fmt.Errorf("rain method must be normalized or native")
	}
	rainMethod.Lock()
	rainMethod.Method = method
	rainMethod.Unlock()

	// Rain keeps transient restore state. Reset the live players when switching so
	// the two implementations are always compared from a clean state.
	scorePlayers.Lock()
	scorePlayers.byRun = make(map[persistentStateKey]*scorePlayerState)
	scorePlayers.Unlock()
	return GetRainMethod(), nil
}

// SetTimingCalibration changes only the runtime experiment. Clearing compiled
// player state makes the new factors take effect coherently on the next frame.
func SetTimingCalibration(slow, fast float64) (TimingCalibration, error) {
	if slow < 1.0 || slow > 3.0 {
		return GetTimingCalibration(), fmt.Errorf("slow duration scale must be between 1.0 and 3.0")
	}
	if fast < 0.2 || fast > 1.0 {
		return GetTimingCalibration(), fmt.Errorf("fast duration scale must be between 0.2 and 1.0")
	}
	timingCalibration.Lock()
	timingCalibration.Slow = slow
	timingCalibration.Fast = fast
	timingCalibration.Unlock()

	scorePlayers.Lock()
	scorePlayers.byRun = make(map[persistentStateKey]*scorePlayerState)
	scorePlayers.Unlock()
	return GetTimingCalibration(), nil
}

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

// trackRepeatDuration uses the common normalized timing experiment for every
// animated effect: authored duration defines the base window and the discrete
// speed preset applies a symmetric duration scale around Normal.
func trackRepeatDuration(l Layer, topology LEDTopology, fanOrdinal, ledCount int) (float64, bool) {
	if strings.EqualFold(l.Effect, "StaticLighting") {
		return 0, true
	}

	// Every animated performer shares the same
	// authored duration + discrete duration scale. The renderer still owns each
	// effect's spatial/shape semantics; this value only says how long one authored
	// effect-second takes in wall time.
	return effectWindowSeconds(l), false
}

// appendScoreNote records a compiled LED command for the static opening state.
// Live effects are evaluated directly and therefore do not use this helper.
func appendScoreNote(notes *[]LEDCommand, l Layer, layerIndex, led int, at float64, color RGB, strength float64) {
	*notes = append(*notes, LEDCommand{
		Time:       at,
		LED:        led,
		Color:      color,
		Strength:   strength,
		LayerIndex: layerIndex,
		Effect:     l.Effect,
	})
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

// auditStaticOpeningFrame reconstructs only the authored StaticLighting opening
// state. It deliberately excludes Rain and every other live performer so the
// diagnostic proves what colour each physical LED should hold before live
// effects begin touching it.
func compileLiveConcert(p *Profile, topology LEDTopology, deviceOrdinal, ledCount int) *compiledScore {
	d := p.LinkDevice()
	if d == nil || len(d.Channels) == 0 {
		return &compiledScore{unsupported: []string{"LINK device entry missing"}}
	}
	layers := p.expandLightingLinks(d.Channels[0].Layers)
	sourceDeviceCount := sourceQXFanCount(layers, topology)
	// Renderer diagnostics intentionally stay out of the frame/compile path.
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
	}
	for name := range unsupported {
		score.unsupported = append(score.unsupported, name)
	}
	sort.Strings(score.unsupported)
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
		firstActive := -1
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
			state.leds[led] = cmd
			changed[led] = struct{}{}
			sum.R += cmd.R
			sum.G += cmd.G
			sum.B += cmd.B
			active++
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
		state = &scorePlayerState{score: score, topology: topology, deviceOrdinal: deviceOrdinal, sourceDeviceCount: sourceCount, leds: make([]RGB, ledCount), rainBase: make([][]RGB, len(score.tracks)), rainLast: make([][]RGB, len(score.tracks)), rainActive: make([][]bool, len(score.tracks))}
		for ti := range score.tracks {
			state.rainBase[ti] = make([]RGB, ledCount)
			state.rainLast[ti] = make([]RGB, ledCount)
			state.rainActive[ti] = make([]bool, ledCount)
		}
		scorePlayers.byRun[key] = state
	}
	elapsed := time.Since(started).Seconds()
	if !state.started {
		_, _ = applyAbsoluteRange(state, 0, elapsed, true)
		state.started = true
	} else if elapsed+0.0000005 < state.lastElapsed {
		for i := range state.leds {
			state.leds[i] = RGB{}
		}
		_, _ = applyAbsoluteRange(state, 0, elapsed, true)
	} else {
		_, _ = applyAbsoluteRange(state, state.lastElapsed, elapsed, false)
	}
	state.lastElapsed = elapsed
	leds := append([]RGB(nil), state.leds...)
	unsupported := append([]string(nil), state.score.unsupported...)
	scorePlayers.Unlock()

	// Dedicated QX mapping overlay: blue references + one purple probe LED.
	// This is deliberately applied after the concert state is advanced so the
	// mapper cannot alter profile playback semantics or persistent renderer state.
	probe := GetMappingProbe()
	if probe.Enabled && topology != nil && strings.EqualFold(topology.Name(), "QX") && deviceOrdinal == probe.DeviceOrdinal && len(leds) == 34 {
		for i := range leds {
			leds[i] = RGB{R: 0, G: 102, B: 255}
		}
		if probe.LocalLED >= 0 && probe.LocalLED < len(leds) {
			leds[probe.LocalLED] = RGB{R: 128, G: 0, B: 255}
		}
	}

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

// effectDurationScale maps iCUE's discrete speed preset onto the shared
// duration clock. Normal is fixed at 1.0x; Slow and Fast come from the
// runtime calibration bench so their exact magnitude can be matched by eye.
func effectDurationScale(l Layer) float64 {
	cal := GetTimingCalibration()
	switch discreteSpeed(l) {
	case 1:
		return cal.Slow
	case 3:
		return cal.Fast
	default:
		return 1.0
	}
}

func effectWindowSeconds(l Layer) float64 {
	durationMS := number(l.Parameters["duration"], 1000)
	if durationMS <= 0 {
		durationMS = 1000
	}
	return (durationMS / 1000.0) * effectDurationScale(l)
}

// effectClock converts wall-clock seconds into the experimental common iCUE
// effect clock. One effective window is one effect-second. Normal is always
// authored duration x 1.0; Slow and Fast use the current runtime calibration
// factors. Effects with no captured duration fall back to 1000 ms. Effect-specific
// geometry (tail, spread, velocity, direction, etc.) remains independent.
func effectClock(l Layer, elapsed float64) float64 {
	window := effectWindowSeconds(l)
	if window <= 0 {
		window = 1
	}
	return elapsed / window
}

// effectProgress is the normalized phase of the shared effect clock. This is
// the OpenLinkHub-style progress model used by periodic effects: wall time is
// first converted by authored duration + Slow/Normal/Fast scaling, then wrapped
// to a stable 0..1 phase for one animation cycle.
//
// Continuous travelling/event effects still use effectClock so they retain the
// cycle count needed for deterministic passes and rain events; their fractional
// phase is mathematically the same effectProgress value.
func effectProgress(l Layer, elapsed float64) float64 {
	phase := math.Mod(effectClock(l, elapsed), 1.0)
	if phase < 0 {
		phase += 1.0
	}
	return phase
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
	clockElapsed := effectClock(l, elapsed)
	phase := effectProgress(l, elapsed)

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
		// Common effect clock: one authored duration is one full blink cycle.
		on := phase < 0.5
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
		// Common effect clock: authored duration defines the full breathe cycle.
		pulse := 0.5 - 0.5*math.Cos(phase*2*math.Pi)
		for i := range c {
			c[i] = col
			a[i] *= pulse
		}
		applyLayerOpacity(l, a)
		return c, a, true
	case "rainlighting":
		if GetRainMethod().Method == "native" {
			// Runtime A/B path for OpenLinkHub's native Rain timing model. Keep
			// this branch isolated from the normalized iCUE Rain implementation.
			c1, c2 := effectColors(l, RGB{R: 25, G: 47, B: 9}, RGB{R: 100, G: 169, B: 45})
			random := boolParam(l.Parameters["random"], false)
			speedFactor := 1.0
			switch discreteSpeed(l) {
			case 1:
				speedFactor = 0.5
			case 2:
				speedFactor = 0.3
			case 3:
				speedFactor = 0.1
			}
			stepDuration := 0.115 / speedFactor
			dropDuration := 0.55 / math.Sqrt(speedFactor)
			spawnChance := 0.18 + (speedFactor-1.0)*0.035
			if spawnChance < 0 {
				spawnChance = 0
			}
			if spawnChance > 1 {
				spawnChance = 1
			}
			currentSlot := int(math.Floor(elapsed / stepDuration))
			for i := 0; i < n; i++ {
				bestIntensity := 0.0
				bestNorm := 0.0
				bestSlot := 0
				for back := 0; back < 6; back++ {
					slot := currentSlot - back
					if slot < 0 || hash(slot, (i+1)*10) > spawnChance {
						continue
					}
					jitter := hash(slot, (i+1)*20) * stepDuration
					age := elapsed - (float64(slot)*stepDuration + jitter)
					if age < 0 || age > dropDuration {
						continue
					}
					norm := age / dropDuration
					intensity := math.Pow(1.0-norm, 1.9)
					intensity *= 0.78 + 0.22*hash(slot, (i+1)*30)
					if intensity > bestIntensity {
						bestIntensity, bestNorm, bestSlot = intensity, norm, slot
					}
				}
				if bestIntensity <= 0 {
					a[i] = 0
					continue
				}
				if random {
					h := hash(bestSlot, (i+1)*41)
					sat := 0.85 + 0.15*hash(bestSlot, (i+1)*42)
					val := 0.90 + 0.10*hash(bestSlot, (i+1)*43)
					c[i] = hsv(h, sat, val)
				} else if bestNorm < 0.5 {
					c[i] = c1
				} else {
					c[i] = c2
				}
				a[i] *= bestIntensity
			}
			applyLayerOpacity(l, a)
			return c, a, true
		}

		// v12.4.38 A/B: model imported iCUE Rain as discrete short-lived drops
		// rather than a continuously travelling full-field waveform.  The cadence
		// and lifetime follow OpenLinkHub's proven native Rain timing model; iCUE's
		// authored colours, selection, direction, random flag and opacity remain in
		// force.  Authored duration is deliberately not used as a spatial sweep
		// period in this A/B.
		c1, c2 := effectColors(l, RGB{R: 25, G: 47, B: 9}, RGB{R: 100, G: 169, B: 45})
		random := boolParam(l.Parameters["random"], false)
		direction := int(number(l.Parameters["direction"], 0))
		// iCUE Rain speed changes how quickly the same-density rain pattern cycles,
		// rather than changing how much rain exists at once. Scale both the spawn
		// cadence and event lifetime by the same factor so their overlap ratio stays
		// constant at Slow/Normal/Fast. The runtime calibration controls scale the
		// shared effect clock globally; Rain does not own a separate speed model.
		// 100 ms is the event cadence measured in normalized effect-clock time.
		stepDuration := 0.100
		// Preserve the calibrated overlap/density ratio while speed changes.
		dropDuration := stepDuration * (0.55 / 0.115)
		currentSlot := int(math.Floor(clockElapsed / stepDuration))
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
				age := clockElapsed - spawnTime
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
		// The common clock has already applied authored duration and speed. Keep
		// the proven Normal propagation at 100 ms per route position and a one-
		// effect-second command lifetime.
		lifetime := 1.0
		stepSeconds := 0.100
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
			front := clockElapsed/stepSeconds + stringPhase(l.ID)*period
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
		// Ripple uses the shared normalized phase. Velocity scales spatial travel
		// while tailLength shapes the falloff; neither creates a second clock.
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
		// Normalize authored velocity around 100: at velocity=100 the ripple
		// traverses one complete route+tail during one shared effect window.
		velocityScale := velocity / 100.0
		radius := math.Mod(phase*velocityScale*(span+tail), span+tail)
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
		// The shared 0..1 phase is the timebase. Treat authored velocity=100 as
		// one complete route traversal per effective duration; other velocity
		// values scale that spatial travel without creating a second clock.
		velocityScale := math.Abs(velocity) / 100.0
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
		head := math.Mod(phase*velocityScale*(span+tail), span+tail)
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
	case "marqueelighting":
		// Marquee keeps the shared normalized clock, but treats geometry as a
		// repeating block/gap pattern. This mirrors OpenLinkHub's cleaner native
		// separation: time advances an offset; block width is purely spatial.
		col := layerColor(l, RGB{R: 255, G: 255, B: 255})
		blockSize := int(math.Round(number(l.Parameters["width"], number(l.Parameters["waveSpread"], 2))))
		if blockSize < 1 {
			blockSize = 1
		}
		gapSize := blockSize
		patternSize := blockSize + gapSize
		direction := int(number(l.Parameters["direction"], 0))
		offset := int(math.Floor(phase * float64(patternSize)))
		if direction == 1 || direction == 3 {
			offset = -offset
		}
		for i := 0; i < n; i++ {
			pos := i + offset
			pos %= patternSize
			if pos < 0 {
				pos += patternSize
			}
			if pos >= blockSize {
				a[i] = 0
				continue
			}
			c[i] = col
		}
		applyLayerOpacity(l, a)
		return c, a, true
	case "sequentiallighting":
		// Sequential uses one normalized cycle to progressively hand the route
		// from the previous colour to the current colour. The clock decides WHEN;
		// LED order decides WHERE. Keeping the cycle count lets two authored
		// colours alternate cleanly without introducing a second timebase.
		c1, c2 := effectColors(l, layerColor(l, RGB{R: 255, G: 255, B: 255}), RGB{})
		clock := effectClock(l, elapsed)
		cycle := int(math.Floor(clock))
		current, previous := c1, RGB{}
		if c2 != (RGB{}) {
			if cycle%2 == 0 {
				current, previous = c1, c2
			} else {
				current, previous = c2, c1
			}
		}
		step := int(math.Floor(phase * float64(n)))
		if step >= n {
			step = n - 1
		}
		direction := int(number(l.Parameters["direction"], 0))
		for i := 0; i < n; i++ {
			route := i
			if direction == 1 || direction == 3 {
				route = n - 1 - i
			}
			if route <= step {
				c[i] = current
			} else if previous != (RGB{}) {
				c[i] = previous
			} else {
				a[i] = 0
			}
		}
		applyLayerOpacity(l, a)
		return c, a, true
	case "visorlighting":
		// Visor is an out-and-back beam. Fold the common phase into a triangle
		// wave for beam position, then apply a smooth Gaussian falloff in LED
		// space. Speed remains exclusively in the shared clock.
		col := layerColor(l, RGB{R: 255, G: 255, B: 255})
		width := number(l.Parameters["width"], number(l.Parameters["waveSpread"], 3))
		if width < 0.5 {
			width = 0.5
		}
		travel := phase * 2
		if travel > 1 {
			travel = 2 - travel
		}
		span := math.Max(float64(n-1), 1)
		head := travel * span
		sigma := math.Max(width, 0.5)
		for i := 0; i < n; i++ {
			dist := math.Abs(float64(i) - head)
			intensity := math.Exp(-0.5 * math.Pow(dist/sigma, 2))
			c[i] = col
			a[i] *= intensity
		}
		applyLayerOpacity(l, a)
		return c, a, true
	case "spirallighting":
		// Spiral uses the same normalized travelling phase as the rainbow spiral,
		// but interpolates the authored colours across the route instead of hue.
		c1, c2 := effectColors(l, RGB{R: 255}, RGB{B: 255})
		clockwise := boolParam(l.Parameters["clockwise"], true)
		effectPhase := phase
		if !clockwise {
			effectPhase = -effectPhase
		}
		for i := 0; i < n; i++ {
			coord := float64(i)
			span := float64(maxInt(n, 1))
			if len(spatial) == n {
				coord = spatial[i]
				span = 17.0
			}
			t := wrapPeriodic(coord/span+effectPhase, 1)
			c[i] = mixRGB(c1, c2, 0.5-0.5*math.Cos(t*2*math.Pi))
		}
		applyLayerOpacity(l, a)
		return c, a, true
	case "colorshiftlighting":
		// Capture-derived structure: two authored colours, speed 1..3 and an
		// optional random flag. Color Shift is a whole-selection transition, not a
		// spatial wave, so every selected LED receives the same interpolated colour.
		c1, c2 := effectColors(l, RGB{R: 255}, RGB{B: 255})
		t := (math.Sin(phase*2*math.Pi-math.Pi/2) + 1) / 2
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
		pulse := 0.5 - 0.5*math.Cos(phase*2*math.Pi)
		col := mixRGB(c1, c2, pulse)
		for i := range c {
			c[i] = col
		}
		applyLayerOpacity(l, a)
		return c, a, true
	case "rainbowwavelighting":
		// Captures expose speed and direction only. Use the known topology
		// coordinate where available and retain a sequential fallback elsewhere.
		direction := int(number(l.Parameters["direction"], 0))
		sign := 1.0
		if direction == 1 || direction == 3 {
			sign = -1
		}
		effectPhase := sign * phase
		for i := 0; i < n; i++ {
			coord := float64(i)
			span := float64(maxInt(n, 1))
			if len(spatial) == n {
				coord = spatial[i]
				span = 17.0
			}
			c[i] = hsv(coord/span+effectPhase, 1, 1)
		}
		applyLayerOpacity(l, a)
		return c, a, true
	case "watercolorlighting":
		// WaterColor captures two authored colours, speed, isRandom and a
		// background colour. Blend broad, slowly moving colour pools rather than
		// inventing discrete flashes; deterministic hashing keeps playback stable.
		c1, c2 := effectColors(l, RGB{R: 255}, RGB{B: 255})
		bg := layerParameterColor(l, "background", RGB{R: 255, G: 255, B: 255})
		effectPhase := phase * 0.40
		random := boolParam(l.Parameters["isRandom"], false)
		for i := 0; i < n; i++ {
			coord := float64(i) / float64(maxInt(n, 1))
			if len(spatial) == n {
				coord = spatial[i] / 17.0
			}
			w1 := 0.5 + 0.5*math.Sin(2*math.Pi*(coord*1.7+effectPhase))
			w2 := 0.5 + 0.5*math.Sin(2*math.Pi*(coord*0.9-effectPhase*0.63+0.31))
			if random {
				w2 = 0.65*w2 + 0.35*hash(int(math.Floor(effectPhase*4)), fanOrdinal*131+i)
			}
			pool := mixRGB(c1, c2, w1)
			c[i] = mixRGB(bg, pool, 0.35+0.65*w2)
		}
		applyLayerOpacity(l, a)
		return c, a, true
	case "rainbowlighting":
		// Rainbow is a whole-selection hue cycle (no direction field in captures).
		col := hsv(phase, 1, 1)
		for i := range c {
			c[i] = col
		}
		applyLayerOpacity(l, a)
		return c, a, true
	case "rainbowchannellighting":
		// Rainbow Channel carries speed+direction and distributes the spectrum
		// across the channel, so it shares Rainbow Wave's spatial basis.
		direction := int(number(l.Parameters["direction"], 0))
		sign := 1.0
		if direction == 1 || direction == 3 {
			sign = -1
		}
		effectPhase := sign * phase
		for i := 0; i < n; i++ {
			coord := float64(i)
			span := float64(maxInt(n, 1))
			if len(spatial) == n {
				coord = spatial[i]
				span = 17.0
			}
			c[i] = hsv(coord/span+effectPhase, 1, 1)
		}
		applyLayerOpacity(l, a)
		return c, a, true
	case "watercolorspectrumlighting":
		// Spectrum variant has no authored endpoint colours. Build overlapping
		// low-saturation spectrum pools from the captured speed/direction tuple.
		direction := int(number(l.Parameters["direction"], 0))
		sign := 1.0
		if direction == 1 || direction == 3 {
			sign = -1
		}
		effectPhase := sign * phase * 0.24
		for i := 0; i < n; i++ {
			coord := float64(i) / float64(maxInt(n, 1))
			if len(spatial) == n {
				coord = spatial[i] / 17.0
			}
			h := coord*0.55 + effectPhase + 0.08*math.Sin(2*math.Pi*(coord+effectPhase*0.4))
			c[i] = hsv(h, 0.68, 1)
		}
		applyLayerOpacity(l, a)
		return c, a, true
	case "spiralrainbowlighting":
		effectPhase := phase
		clockwise := boolParam(l.Parameters["clockwise"], true)
		if !clockwise {
			effectPhase = -effectPhase
		}
		for i := 0; i < n; i++ {
			coord := float64(i)
			span := float64(maxInt(n, 1))
			if len(spatial) == n {
				coord = spatial[i]
				span = 17.0
			}
			hue := math.Mod(coord/span+effectPhase, 1)
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
			t := phase
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
		t := (math.Sin(phase*2*math.Pi) + 1) / 2
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

// EnrichAuthoredSourceColors decodes colours captured directly from each
// authored .cueprofile layer. This deliberately bypasses compiled playback and
// composition: the editor should display/edit the value stored on the layer in
// the source profile itself. RawColorValues remains untouched for lossless
// writer/reverse-engineering work.
func EnrichAuthoredSourceColors(layers []Layer) []Layer {
	out := make([]Layer, len(layers))
	copy(out, layers)
	for i := range out {
		// Always rebuild the derived list from the preserved source leaves. This
		// keeps reloads/idempotent enrichment from accumulating stale colours.
		out[i].SourceColors = nil
		seen := make(map[string]bool)
		for _, raw := range out[i].RawColorValues {
			c, ok := parseAuthoredColorLeaf(raw)
			if !ok {
				continue
			}
			hex := editorColor(c).Hex
			key := strings.ToLower(hex)
			if !seen[key] {
				seen[key] = true
				out[i].SourceColors = append(out[i].SourceColors, hex)
			}
		}
	}
	return out
}

// parseAuthoredColorLeaf decodes one exact leaf captured from the authored
// .cueprofile. iCUE commonly stores these as "color=#AARRGGBB" alongside
// non-colour leaves such as "time=0". Keep this deliberately narrow: only a
// colour-labelled leaf or a bare colour value is accepted, so timing/numeric
// metadata can never be mistaken for a colour.
func parseAuthoredColorLeaf(raw string) (RGB, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return RGB{}, false
	}
	if eq := strings.Index(s, "="); eq >= 0 {
		key := strings.ToLower(strings.TrimSpace(s[:eq]))
		if key != "color" && key != "colour" && key != "first" && key != "second" && key != "value" {
			return RGB{}, false
		}
		s = strings.TrimSpace(s[eq+1:])
	}
	return parseColor(s)
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
	Index              int                          `json:"index"`
	Name               string                       `json:"name"`
	Effect             string                       `json:"effect"`
	EffectName         string                       `json:"effectName,omitempty"`
	Enabled            bool                         `json:"enabled"`
	SelectionMode      string                       `json:"selectionMode,omitempty"`
	LEDs               []string                     `json:"leds,omitempty"`
	Colors             []EditorColor                `json:"colors"`
	PrimaryColor       *EditorColor                 `json:"primaryColor,omitempty"`
	RawParameters      map[string]any               `json:"rawParameters,omitempty"`
	AuthoredParameters map[string]AuthoredParameter `json:"authoredParameters,omitempty"`
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

func rendererDecodedLayerColors(l Layer) []EditorColor {
	// The editor must not independently interpret Corsair serialization. Ask the
	// exact same helpers used by renderLayer for the authored effect colours and
	// use NaN sentinels so a missing colour is distinguishable from authored black.
	missing := RGB{R: math.NaN(), G: math.NaN(), B: math.NaN()}
	valid := func(c RGB) bool {
		return !math.IsNaN(c.R) && !math.IsNaN(c.G) && !math.IsNaN(c.B)
	}
	appendUnique := func(out []EditorColor, c RGB) []EditorColor {
		if !valid(c) {
			return out
		}
		e := editorColor(c)
		for _, existing := range out {
			if strings.EqualFold(existing.Hex, e.Hex) {
				return out
			}
		}
		return append(out, e)
	}

	var out []EditorColor
	switch strings.ToLower(l.Effect) {
	case "staticlighting", "blinklighting", "breathelighting", "marqueelighting", "visorlighting":
		out = appendUnique(out, layerColor(l, missing))
	default:
		c1, c2 := effectColors(l, missing, missing)
		out = appendUnique(out, c1)
		out = appendUnique(out, c2)
		// Several single-colour effects use layerColor rather than effectColors.
		// This fallback is still renderer-owned decoding, not browser inference.
		if len(out) == 0 {
			out = appendUnique(out, layerColor(l, missing))
		}
	}
	return out
}

// DecodeEditorLayers exposes the renderer's canonical authored values without
// changing the parsed Layer objects used for playback. RawParameters remains
// alongside the decoded model so the workspace can always show exactly what
// the source profile contained.
func DecodeEditorLayers(layers []Layer) []EditorLayer {
	out := make([]EditorLayer, 0, len(layers))
	for i, l := range layers {
		colors := rendererDecodedLayerColors(l)
		e := EditorLayer{Index: i, Name: l.Name, Effect: l.Effect, EffectName: l.EffectName, Enabled: l.Enabled, SelectionMode: l.SelectionMode, LEDs: append([]string(nil), l.LEDs...), Colors: colors, RawParameters: l.Parameters, AuthoredParameters: l.AuthoredParameters}
		if len(colors) > 0 {
			c := colors[0]
			e.PrimaryColor = &c
		}
		out = append(out, e)
	}
	return out
}

// DecodeEditorProfileLayers builds the workspace model from the same expanded
// authored layers and compile path used by LINK playback. In particular, static
// colours are taken from the actual compiled opening notes, before layers are
// composited into a device frame. This keeps XML/authored layer identity intact
// while making the editor consume the exact value the renderer compiled.
func DecodeEditorProfileLayers(p *Profile) []EditorLayer {
	if p == nil {
		return nil
	}
	authored := p.LightingLinkLayers
	if len(authored) == 0 {
		return nil
	}

	// Use the real LINK LightingLink wrapper when present so selection semantics
	// match playback. The expanded slice preserves authored LightingLink order,
	// therefore xml index remains the stable bridge back to the editor layer.
	compiled := authored
	if d := p.LinkDevice(); d != nil && len(d.Channels) > 0 {
		expanded := p.expandLightingLinks(d.Channels[0].Layers)
		if len(expanded) == len(authored) {
			compiled = expanded
		}
	}

	out := make([]EditorLayer, 0, len(authored))
	topology := LEDTopology(qxTopology{})
	sourceDeviceCount := sourceQXFanCount(compiled, topology)
	for i, source := range authored {
		renderLayerSource := source
		if i < len(compiled) {
			renderLayerSource = compiled[i]
		}

		colors := rendererDecodedLayerColors(renderLayerSource)
		if strings.EqualFold(renderLayerSource.Effect, "StaticLighting") {
			// Static is the strongest proof point: recordLayerTrack is the exact
			// compiler used by playback. Pull unique colours from its notes rather
			// than decoding Parameters a second time.
			notes, ok := recordLayerTrack(renderLayerSource, i, 0, topology.LEDCount(), 0, topology, sourceDeviceCount)
			if ok && len(notes) > 0 {
				colors = colors[:0]
				seen := map[string]bool{}
				for _, note := range notes {
					c := editorColor(note.Color)
					if !seen[strings.ToLower(c.Hex)] {
						seen[strings.ToLower(c.Hex)] = true
						colors = append(colors, c)
					}
				}
			}
		}

		e := EditorLayer{Index: i, Name: source.Name, Effect: source.Effect, EffectName: source.EffectName, Enabled: source.Enabled, SelectionMode: source.SelectionMode, LEDs: append([]string(nil), source.LEDs...), Colors: colors, RawParameters: source.Parameters, AuthoredParameters: source.AuthoredParameters}
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
