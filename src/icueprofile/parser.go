package icueprofile

import (
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
)

// Node is a deliberately small generic XML tree. Corsair's cereal XML contains
// many polymorphic shapes, so the diagnostic parser keeps unknown fields rather
// than binding the whole format to a brittle set of structs.
type Node struct {
	XMLName  xml.Name
	Text     string
	Attrs    []xml.Attr
	Children []*Node
}

func (n *Node) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	n.XMLName = start.Name
	n.Attrs = append([]xml.Attr(nil), start.Attr...)
	var text strings.Builder
	for {
		tok, err := d.Token()
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			child := &Node{}
			if err := d.DecodeElement(child, &t); err != nil {
				return err
			}
			n.Children = append(n.Children, child)
		case xml.CharData:
			text.Write([]byte(t))
		case xml.EndElement:
			n.Text = strings.TrimSpace(text.String())
			return nil
		}
	}
}

type Profile struct {
	Name    string   `json:"name"`
	ID      string   `json:"id,omitempty"`
	Devices []Device `json:"devices"`
	Summary Summary  `json:"summary"`
}

type Device struct {
	DeviceID           string    `json:"deviceId,omitempty"`
	USBVendorID        int       `json:"usbVid,omitempty"`
	USBProductID       int       `json:"usbPid,omitempty"`
	HIDCaps            string    `json:"hidCaps,omitempty"`
	DeviceLightingType string    `json:"deviceLightingType,omitempty"`
	Channels           []Channel `json:"channels"`
}

type Channel struct {
	Index  string  `json:"index"`
	Layers []Layer `json:"layers"`
}

type Layer struct {
	Name          string         `json:"name"`
	ID            string         `json:"id,omitempty"`
	Enabled       bool           `json:"enabled"`
	SelectionMode string         `json:"selectionMode,omitempty"`
	LEDs          []string       `json:"leds,omitempty"`
	Effect        string         `json:"effect"`
	EffectName    string         `json:"effectName,omitempty"`
	Parameters    map[string]any `json:"parameters,omitempty"`
}

type Summary struct {
	DeviceEntries int            `json:"deviceEntries"`
	Channels      int            `json:"channels"`
	Layers        int            `json:"layers"`
	EnabledLayers int            `json:"enabledLayers"`
	Effects       map[string]int `json:"effects"`
	LEDSelections map[string]int `json:"ledSelectionModes"`
}

func ParseFile(path string) (*Profile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Parse(f)
}

func Parse(r io.Reader) (*Profile, error) {
	dec := xml.NewDecoder(r)
	var root Node
	if err := dec.Decode(&root); err != nil {
		return nil, fmt.Errorf("parse iCUE XML: %w", err)
	}

	profileNode := firstDescendant(&root, "profile")
	if profileNode == nil {
		return nil, fmt.Errorf("iCUE profile node not found")
	}

	typeNames := buildPolymorphicTypeMap(&root)

	out := &Profile{
		Name:    childText(profileNode, "name"),
		ID:      childText(profileNode, "id"),
		Summary: Summary{Effects: map[string]int{}, LEDSelections: map[string]int{}},
	}

	walk(profileNode, func(n *Node) {
		key := directChild(n, "key")
		value := directChild(n, "value")
		if key == nil || value == nil || !looksLikeDeviceKey(key) {
			return
		}
		dev := parseDevice(key, value, typeNames)
		if len(dev.Channels) == 0 {
			return
		}
		out.Devices = append(out.Devices, dev)
	})

	// Cereal can contain duplicate/default device entries. Keep all of them in
	// diagnostics because that is valuable while reverse engineering.
	out.Summary.DeviceEntries = len(out.Devices)
	for _, d := range out.Devices {
		out.Summary.Channels += len(d.Channels)
		for _, c := range d.Channels {
			for _, l := range c.Layers {
				out.Summary.Layers++
				if l.Enabled {
					out.Summary.EnabledLayers++
				}
				out.Summary.Effects[l.Effect]++
				out.Summary.LEDSelections[l.SelectionMode]++
			}
		}
	}
	return out, nil
}

func parseDevice(key, value *Node, typeNames map[int]string) Device {
	model := firstDescendant(key, "modelId")
	d := Device{
		DeviceID:           childText(key, "deviceId"),
		HIDCaps:            childText(key, "hidCaps"),
		DeviceLightingType: childText(key, "deviceLightingType"),
	}
	if model != nil {
		d.USBVendorID = parseInt(childText(model, "usbVid"))
		d.USBProductID = parseInt(childText(model, "usbPid"))
	}

	// channelProperties is a map encoded as valueN -> key + value.
	cp := firstDescendant(value, "channelProperties")
	if cp == nil {
		return d
	}
	for _, entry := range cp.Children {
		idx := childText(entry, "key")
		channelValue := directChild(entry, "value")
		if channelValue == nil {
			continue
		}
		layersNode := firstDescendant(channelValue, "layers")
		if layersNode == nil {
			continue
		}
		ch := Channel{Index: idx}
		for _, layerEntry := range layersNode.Children {
			if layer := parseLayer(layerEntry, typeNames); layer != nil {
				ch.Layers = append(ch.Layers, *layer)
			}
		}
		if len(ch.Layers) > 0 {
			d.Channels = append(d.Channels, ch)
		}
	}
	return d
}

func parseLayer(entry *Node, typeNames map[int]string) *Layer {
	data := firstPath(entry, "ptr_wrapper", "data")
	if data == nil {
		return nil
	}
	base := directChild(data, "base")
	lighting := directChild(data, "lighting")
	if base == nil || lighting == nil {
		return nil
	}

	l := &Layer{
		Name:          childText(base, "name"),
		ID:            childText(base, "id"),
		Enabled:       strings.EqualFold(childText(base, "enabled"), "true") || childText(base, "enabled") == "1",
		SelectionMode: childText(base, "option"),
	}
	if keys := directChild(base, "keys"); keys != nil {
		for _, k := range keys.Children {
			if s := strings.TrimSpace(k.Text); s != "" {
				l.LEDs = append(l.LEDs, s)
			}
		}
	}

	l.Effect = childText(lighting, "polymorphic_name")
	if l.Effect == "" {
		l.Effect = typeNames[polymorphicTypeID(childText(lighting, "polymorphic_id"))]
	}
	effectData := firstPath(lighting, "ptr_wrapper", "data")
	if effectData != nil {
		if l.Effect == "" {
			l.Effect = childText(effectData, "polymorphic_name")
		}
		l.EffectName = findFirstField(effectData, "name")
		l.Parameters = extractParameters(effectData)
	}
	if l.Effect == "" {
		l.Effect = "UnknownLighting"
	}
	return l
}

func extractParameters(effect *Node) map[string]any {
	wanted := map[string]bool{
		"duration": true, "brightness": true, "opacity": true, "speed": true,
		"direction": true, "clockwise": true, "random": true, "color": true,
		"colors": true, "transitions": true,
	}
	result := map[string]any{}
	for key := range wanted {
		nodes := descendants(effect, key)
		if len(nodes) == 0 {
			continue
		}
		if key == "transitions" || key == "colors" {
			vals := collectLeafValues(nodes[0])
			if len(vals) > 0 {
				result[key] = vals
			}
			continue
		}
		vals := make([]string, 0, len(nodes))
		for _, n := range nodes {
			if strings.TrimSpace(n.Text) != "" {
				vals = append(vals, strings.TrimSpace(n.Text))
			}
		}
		if len(vals) == 1 {
			result[key] = scalar(vals[0])
		} else if len(vals) > 1 {
			result[key] = vals
		}
	}
	return result
}

func scalar(s string) any {
	// Numeric cereal fields commonly use 0/1. Parse numbers before booleans so
	// values such as RainLighting speed=1 remain numeric diagnostics.
	if i, err := strconv.Atoi(s); err == nil {
		return i
	}
	if b, err := strconv.ParseBool(s); err == nil {
		return b
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	return s
}

func collectLeafValues(n *Node) []string {
	var out []string
	walk(n, func(x *Node) {
		if len(x.Children) == 0 && strings.TrimSpace(x.Text) != "" {
			out = append(out, x.XMLName.Local+"="+strings.TrimSpace(x.Text))
		}
	})
	return out
}

func buildPolymorphicTypeMap(root *Node) map[int]string {
	out := map[int]string{}
	walk(root, func(n *Node) {
		idNode := directChild(n, "polymorphic_id")
		nameNode := directChild(n, "polymorphic_name")
		if idNode == nil || nameNode == nil || strings.TrimSpace(nameNode.Text) == "" {
			return
		}
		id := polymorphicTypeID(idNode.Text)
		if id != 0 {
			out[id] = strings.TrimSpace(nameNode.Text)
		}
	})
	return out
}

func polymorphicTypeID(s string) int {
	v, err := strconv.ParseUint(strings.TrimSpace(s), 10, 32)
	if err != nil {
		return 0
	}
	// cereal marks a newly declared polymorphic type with bit 31. Later
	// references use the same id without that declaration bit.
	return int(v & 0x7fffffff)
}

func looksLikeDeviceKey(n *Node) bool {
	return directChild(n, "deviceId") != nil && directChild(n, "modelId") != nil && directChild(n, "hidCaps") != nil
}

func parseInt(s string) int { i, _ := strconv.Atoi(strings.TrimSpace(s)); return i }
func directChild(n *Node, name string) *Node {
	for _, c := range n.Children {
		if c.XMLName.Local == name {
			return c
		}
	}
	return nil
}
func childText(n *Node, name string) string {
	if c := directChild(n, name); c != nil {
		return strings.TrimSpace(c.Text)
	}
	return ""
}
func firstPath(n *Node, names ...string) *Node {
	cur := n
	for _, name := range names {
		cur = directChild(cur, name)
		if cur == nil {
			return nil
		}
	}
	return cur
}
func firstDescendant(n *Node, name string) *Node {
	var found *Node
	walk(n, func(x *Node) {
		if found == nil && x.XMLName.Local == name {
			found = x
		}
	})
	return found
}
func descendants(n *Node, name string) []*Node {
	var out []*Node
	walk(n, func(x *Node) {
		if x.XMLName.Local == name {
			out = append(out, x)
		}
	})
	return out
}
func findFirstField(n *Node, name string) string {
	if x := firstDescendant(n, name); x != nil {
		return strings.TrimSpace(x.Text)
	}
	return ""
}
func walk(n *Node, fn func(*Node)) {
	fn(n)
	for _, c := range n.Children {
		walk(c, fn)
	}
}

// EffectNames returns a stable sorted list useful for diagnostics and tests.
func (p *Profile) EffectNames() []string {
	out := make([]string, 0, len(p.Summary.Effects))
	for k := range p.Summary.Effects {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
