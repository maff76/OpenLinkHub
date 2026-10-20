package icueprofile

import (
	"sort"
	"strings"
)

// EffectSupport describes one iCUE effect found in an imported profile.
type EffectSupport struct {
	Effect string `json:"effect"`
	Layers int    `json:"layers"`
}

// Compatibility is a renderer capability report for an imported profile.
// It deliberately reports only what the current generic renderer implements;
// device/topology fidelity is a separate concern.
type Compatibility struct {
	Status          string          `json:"status"`
	SupportedLayers int             `json:"supportedLayers"`
	TotalLayers     int             `json:"totalLayers"`
	Supported       []EffectSupport `json:"supported"`
	Unsupported     []EffectSupport `json:"unsupported"`
}

// IsSupportedEffect is the single capability registry used by compatibility
// reporting. Keep this in sync with renderLayer in render.go.
func IsSupportedEffect(effect string) bool {
	switch strings.ToLower(strings.TrimSpace(effect)) {
	case "basicnonelighting",
		"staticlighting",
		"blinklighting",
		"breathelighting",
		"rainlighting",
		"spiralrainbowlighting",
		"gradientlighting",
		"colorwavelighting",
		"wavelighting",
		"colorshiftlighting",
		"colorpulselighting",
		"ripplelighting",
		"spirallighting",
		"visorlighting",
		"marqueelighting",
		"sequentiallighting",
		"rainbowwavelighting",
		"watercolorlighting",
		"rainbowlighting",
		"rainbowchannellighting",
		"watercolorspectrumlighting",
		"lightinglink":
		return true
	default:
		return false
	}
}

// BuildCompatibility reports which effect types in a parsed profile are
// implemented by the current generic iCUE renderer.
func BuildCompatibility(effects map[string]int) Compatibility {
	out := Compatibility{}
	for effect, layers := range effects {
		out.TotalLayers += layers
		item := EffectSupport{Effect: effect, Layers: layers}
		if IsSupportedEffect(effect) {
			out.SupportedLayers += layers
			out.Supported = append(out.Supported, item)
		} else {
			out.Unsupported = append(out.Unsupported, item)
		}
	}
	sort.Slice(out.Supported, func(i, j int) bool {
		return strings.ToLower(out.Supported[i].Effect) < strings.ToLower(out.Supported[j].Effect)
	})
	sort.Slice(out.Unsupported, func(i, j int) bool {
		return strings.ToLower(out.Unsupported[i].Effect) < strings.ToLower(out.Unsupported[j].Effect)
	})

	switch {
	case len(out.Unsupported) == 0:
		out.Status = "fully_supported"
	case len(out.Supported) == 0:
		out.Status = "unsupported"
	default:
		out.Status = "partially_supported"
	}
	return out
}
