package motherboards

// Package: motherboards
// Author: Nikola Jurkovic
// License: GPL-3.0 or later

import (
	"OpenLinkHub/src/common"
	"OpenLinkHub/src/config"
	"OpenLinkHub/src/logger"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

type Headers struct {
	Id           int            `json:"id"`
	HeaderName   string         `json:"headerName"`
	HeaderInput  string         `json:"headerInput"`
	HeaderConfig string         `json:"headerConfig"`
	HeaderLabel  string         `json:"headerLabel"`
	HeaderModes  map[int]string `json:"headerModes"`
	HeaderValue  string         `json:"headerValue"`
}
type Motherboard struct {
	Name        string          `json:"name"`
	DisplayName string          `json:"displayName"`
	Chip        string          `json:"chip"`
	Interval    float32         `json:"interval"`
	Headers     map[int]Headers `json:"headers"`
	Discovery   string          `json:"discovery,omitempty"`
}
type Motherboards struct {
	Entry        string        `json:"entry"`
	Motherboards []Motherboard `json:"motherboards"`
}

var (
	pwd         = ""
	boardName   = ""
	hwmonPath   = ""
	boardSerial = ""
	motherboard Motherboards
	mutex       sync.Mutex
)

func Init() {
	pwd = config.GetConfig().ConfigPath

	// Read actual board name from DMI. The board identity is used for display,
	// stable serial generation and optional legacy overrides, not as a
	// prerequisite for discovering motherboard fan controls.
	dmiBoardNamePath := "/sys/class/dmi/id/board_name"
	if b, err := os.ReadFile(dmiBoardNamePath); err != nil {
		logger.Log(logger.Fields{"error": err, "location": dmiBoardNamePath}).Warn("Unable to read system board name")
	} else {
		boardName = strings.TrimSpace(string(b))
		sum := md5.Sum([]byte(boardName))
		boardSerial = hex.EncodeToString(sum[:])
	}

	// Keep the existing database as an optional compatibility/override source.
	// Automatic sysfs discovery below is authoritative for which channels
	// actually exist.
	location := pwd + "/database/motherboard/motherboard.json"
	if file, err := os.Open(location); err == nil {
		defer file.Close()
		reader := json.NewDecoder(file)
		if err := reader.Decode(&motherboard); err != nil {
			logger.Log(logger.Fields{"error": err, "location": location}).Warn("Unable to decode motherboard file")
		}
	} else {
		logger.Log(logger.Fields{"error": err, "location": location}).Warn("Unable to open motherboard file; using automatic motherboard discovery")
	}

	legacy := getConfiguredMotherboard()
	discovered, path := discoverMotherboard(legacy)
	if discovered != nil {
		hwmonPath = path
		motherboard.Motherboards = []Motherboard{*discovered}
		logger.Log(logger.Fields{
			"board":     boardName,
			"chip":      discovered.Chip,
			"headers":   len(discovered.Headers),
			"path":      hwmonPath,
			"discovery": discovered.Discovery,
		}).Info("Motherboard fan headers discovered from hwmon")
		logDiscoveredHeaders(discovered, hwmonPath)
		return
	}

	// Fall back to the legacy board definition if automatic discovery could
	// not prove a complete fan/PWM control mapping.
	if legacy != nil {
		hwmonPath = findHwmonByChip(motherboard.Entry, legacy.Chip)
		if hwmonPath != "" {
			for k, v := range legacy.Headers {
				headerLabel := readHeaderLabel(hwmonPath, v.HeaderLabel)
				if headerLabel != "" {
					v.HeaderName = headerLabel
					legacy.Headers[k] = v
				}
			}
			legacy.Discovery = "legacy"
			motherboard.Motherboards = []Motherboard{*legacy}
			logger.Log(logger.Fields{
				"board": boardName, "chip": legacy.Chip, "path": hwmonPath, "discovery": legacy.Discovery,
			}).Info("Using legacy motherboard definition")
		}
	}
}

// getConfiguredMotherboard returns the optional board-specific definition.
func getConfiguredMotherboard() *Motherboard {
	for i := range motherboard.Motherboards {
		if motherboard.Motherboards[i].Name == boardName {
			return &motherboard.Motherboards[i]
		}
	}
	return nil
}

// discoverMotherboard scans hwmon for a controller exposing complete matching
// fanN_input + pwmN + pwmN_enable triplets. RPM-only channels are deliberately
// ignored because OpenLinkHub must never guess a writable PWM mapping.
func discoverMotherboard(legacy *Motherboard) (*Motherboard, string) {
	base := strings.TrimSpace(motherboard.Entry)
	if base == "" {
		base = "/sys/class/hwmon/"
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		logger.Log(logger.Fields{"base": base, "error": err}).Warn("Unable to scan hwmon for motherboard fan controls")
		return nil, ""
	}

	type candidate struct {
		board        *Motherboard
		path         string
		score        int
		platformPath bool
		labeled      int
	}
	var candidates []candidate
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "hwmon") {
			continue
		}
		path := filepath.Join(base, entry.Name())
		nameBytes, err := os.ReadFile(filepath.Join(path, "name"))
		if err != nil {
			continue
		}
		chip := strings.TrimSpace(string(nameBytes))
		// A complete fanN_input + pwmN + pwmN_enable mapping is the capability
		// test. Do not require a pre-approved controller name: this lets new
		// hwmon-compatible Super-I/O drivers work without a code/database update.
		headers := discoverHeaders(path, chip, legacy)
		if len(headers) == 0 {
			continue
		}
		displayName := boardName
		interval := float32(3000)
		discovery := "automatic"
		if legacy != nil {
			if legacy.DisplayName != "" {
				displayName = legacy.DisplayName
			}
			if legacy.Interval > 0 {
				interval = legacy.Interval
			}
			discovery = "automatic+override"
		}
		platformPath := isPlatformHwmonPath(path)
		labeled := labeledHeaderCount(path, headers)
		score := len(headers) * 20
		if platformPath {
			// Super-I/O motherboard fan controllers are normally platform
			// devices. Prefer that hardware topology without requiring a
			// controller-name allowlist.
			score += 150
		}
		score += labeled * 5
		if legacy != nil && chip == legacy.Chip {
			// A matching legacy definition is supporting evidence only; it
			// must never create channels that sysfs did not prove exist.
			score += 50
		}

		candidates = append(candidates, candidate{
			board: &Motherboard{
				Name: boardName, DisplayName: displayName, Chip: chip,
				Interval: interval, Headers: headers, Discovery: discovery,
			},
			path: path, platformPath: platformPath, labeled: labeled, score: score,
		})
		logger.Log(logger.Fields{
			"chip": chip, "path": path, "headers": len(headers),
			"labeledHeaders": labeled, "platformDevice": platformPath, "score": score,
		}).Info("Motherboard fan-control candidate discovered")
	}
	if len(candidates) == 0 {
		return nil, ""
	}

	// Rank by hardware evidence rather than controller name. A platform-backed
	// complete mapping is strong evidence of the board Super-I/O controller;
	// channel count, kernel labels and an optional legacy chip match add further
	// confidence. This avoids selecting an unrelated PCI/USB PWM device merely
	// because it exposes more channels, while remaining open to new hwmon drivers.
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		li, lj := len(candidates[i].board.Headers), len(candidates[j].board.Headers)
		if li != lj {
			return li > lj
		}
		return candidates[i].board.Chip < candidates[j].board.Chip
	})
	selected := candidates[0]
	logger.Log(logger.Fields{
		"chip": selected.board.Chip, "path": selected.path,
		"headers": len(selected.board.Headers), "labeledHeaders": selected.labeled,
		"platformDevice": selected.platformPath, "score": selected.score,
		"candidates": len(candidates),
	}).Info("Selected motherboard fan-control candidate")
	return selected.board, selected.path
}

// isPlatformHwmonPath checks hardware topology rather than a controller-name
// allowlist. Linux Super-I/O motherboard monitoring drivers are commonly bound
// below /sys/devices/platform. Failure to resolve the symlink is non-fatal.
func isPlatformHwmonPath(path string) bool {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	clean := filepath.ToSlash(filepath.Clean(resolved))
	return strings.Contains(clean, "/devices/platform/")
}

func labeledHeaderCount(path string, headers map[int]Headers) int {
	count := 0
	for _, header := range headers {
		if readHeaderLabel(path, header.HeaderLabel) != "" {
			count++
		}
	}
	return count
}

func discoverHeaders(path, chip string, legacy *Motherboard) map[int]Headers {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil
	}
	var physical []int
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "fan") || !strings.HasSuffix(name, "_input") {
			continue
		}
		indexText := strings.TrimSuffix(strings.TrimPrefix(name, "fan"), "_input")
		index, err := strconv.Atoi(indexText)
		if err != nil || index < 1 {
			continue
		}
		if !common.FileExists(filepath.Join(path, fmt.Sprintf("pwm%d", index))) ||
			!common.FileExists(filepath.Join(path, fmt.Sprintf("pwm%d_enable", index))) {
			continue
		}
		physical = append(physical, index)
	}
	sort.Ints(physical)

	headers := make(map[int]Headers, len(physical))
	for logical, index := range physical {
		id := logical + 1
		labelFile := fmt.Sprintf("fan%d_label", index)
		name := readHeaderLabel(path, labelFile)
		if name == "" {
			name = fmt.Sprintf("Fan %d", id)
		}
		modes := headerModesForChip(chip)
		// Preserve a known board's mode semantics when available, but never its
		// channel existence or sysfs mapping.
		if legacy != nil {
			if old, ok := legacy.Headers[id]; ok && len(old.HeaderModes) > 0 {
				modes = old.HeaderModes
			}
		}
		headers[id] = Headers{
			Id: id, HeaderName: name,
			HeaderInput:  fmt.Sprintf("fan%d_input", index),
			HeaderConfig: fmt.Sprintf("pwm%d_enable", index),
			HeaderLabel:  labelFile,
			HeaderModes:  modes,
			HeaderValue:  fmt.Sprintf("pwm%d", index),
		}
	}
	return headers
}

func headerModesForChip(chip string) map[int]string {
	// pwmN_enable values are driver-defined. Linux hwmon commonly uses 1 for
	// manual PWM and 2 for automatic/firmware control. The nct679x family uses
	// Smart Fan mode 5. Board-specific JSON remains available as an override
	// when a driver exposes different semantics.
	name := strings.ToLower(strings.TrimSpace(chip))
	if strings.HasPrefix(name, "nct679") {
		return map[int]string{1: "PWM", 5: "BIOS"}
	}
	return map[int]string{1: "PWM", 2: "BIOS"}
}

func logDiscoveredHeaders(board *Motherboard, path string) {
	if board == nil {
		return
	}
	ids := make([]int, 0, len(board.Headers))
	for id := range board.Headers {
		ids = append(ids, id)
	}
	sort.Ints(ids)

	for _, id := range ids {
		header := board.Headers[id]
		logger.Log(logger.Fields{
			"header":     id,
			"name":       header.HeaderName,
			"rpm":        header.HeaderInput,
			"pwm":        header.HeaderValue,
			"pwmEnable":  header.HeaderConfig,
			"label":      header.HeaderLabel,
			"hwmonPath":  path,
			"controller": board.Chip,
		}).Info("Motherboard fan header discovered")
	}
}

func readHeaderLabel(path, labelFile string) string {
	if strings.TrimSpace(labelFile) == "" {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(path, strings.TrimPrefix(labelFile, "/")))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// GetMotherboard will return motherboard by its name
func GetMotherboard() *Motherboard {
	for i := range motherboard.Motherboards {
		if motherboard.Motherboards[i].Name == boardName {
			return &motherboard.Motherboards[i]
		}
	}
	if len(motherboard.Motherboards) == 1 {
		return &motherboard.Motherboards[0]
	}
	return nil
}

// GetMotherboardSerial will return motherboard serial
func GetMotherboardSerial() string {
	return boardSerial
}

// GetMotherboardPath will return motherboard hwmon path
func GetMotherboardPath() string {
	return hwmonPath
}

// SetMotherboardHeaderMode will set motherboard header mode
func SetMotherboardHeaderMode(header, mode int) uint8 {
	mutex.Lock()
	defer mutex.Unlock()

	m := GetMotherboard()
	if m == nil {
		return 0
	}

	if val, ok := m.Headers[header]; ok {
		if _, valid := val.HeaderModes[mode]; !valid {
			logger.Log(logger.Fields{"mode": mode, "header": header}).Warn("Invalid PWM header mode")
			return 0
		}
		pwmConfig := filepath.Join(hwmonPath, strings.TrimPrefix(val.HeaderConfig, "/"))
		if common.FileExists(pwmConfig) {
			err := os.WriteFile(pwmConfig, []byte(fmt.Sprintf("%d\n", mode)), 0)
			if err != nil {
				logger.Log(logger.Fields{"mode": mode, "header": header, "error": err}).Warn("Unable to set PWM header mode")
				return 0
			}
			return 1
		}
	}
	return 0
}

// GetMotherboardHeaderMode will return motherboard header mode
func GetMotherboardHeaderMode(header int) int {
	mutex.Lock()
	defer mutex.Unlock()

	m := GetMotherboard()
	if m == nil {
		return 0
	}

	val, ok := m.Headers[header]
	if !ok {
		return 0
	}

	pwmConfig := filepath.Join(hwmonPath, strings.TrimPrefix(val.HeaderConfig, "/"))

	b, err := os.ReadFile(pwmConfig)
	if err != nil {
		logger.Log(logger.Fields{"header": header, "path": pwmConfig, "error": err}).Warn("Unable to read header config value")
		return 0
	}

	s := strings.TrimSpace(string(b))
	n, err := strconv.Atoi(s)
	if err != nil {
		logger.Log(logger.Fields{"header": header, "path": pwmConfig, "raw": s, "error": err}).Warn("Unable to parse header config value")
		return 0
	}

	if n < 0 {
		return 0
	}

	if _, valid := val.HeaderModes[n]; !valid {
		return 0
	}

	return n
}

// GetMotherboardHeaderLabel will return motherboard header label
func GetMotherboardHeaderLabel(header int) string {
	mutex.Lock()
	defer mutex.Unlock()

	m := GetMotherboard()
	if m == nil {
		return ""
	}

	val, ok := m.Headers[header]
	if !ok {
		return ""
	}

	headerLabel := filepath.Join(hwmonPath, strings.TrimPrefix(val.HeaderLabel, "/"))

	if common.FileExists(headerLabel) {
		b, err := os.ReadFile(headerLabel)
		if err != nil {
			logger.Log(logger.Fields{"header": header, "path": headerLabel, "error": err}).Warn("Unable to read header label value")
			return ""
		}

		return strings.TrimSpace(string(b))
	}
	return ""
}

// SetMotherboardHeaderValue will set motherboard header value
func SetMotherboardHeaderValue(header, value int) uint8 {
	mutex.Lock()
	defer mutex.Unlock()

	if value < 1 || value > 100 {
		logger.Log(logger.Fields{"value": value, "header": header}).Warn("Invalid PWM header value")
		return 0
	}

	m := GetMotherboard()
	if m == nil {
		return 0
	}

	if val, ok := m.Headers[header]; ok {
		pwmValue := filepath.Join(hwmonPath, strings.TrimPrefix(val.HeaderValue, "/"))
		valToByte := percentToByte(value)
		if common.FileExists(pwmValue) {
			err := os.WriteFile(pwmValue, []byte(fmt.Sprintf("%d\n", valToByte)), 0)
			if err != nil {
				logger.Log(logger.Fields{"value": valToByte, "header": header, "error": err}).Warn("Unable to set PWM header value")
				return 0
			}
			return 1
		}
	}
	return 0
}

// GetMotherboardHeaderValue will get motherboard header value
func GetMotherboardHeaderValue(header int) int16 {
	mutex.Lock()
	defer mutex.Unlock()

	m := GetMotherboard()
	if m == nil {
		return 0
	}

	val, ok := m.Headers[header]
	if !ok {
		return 0
	}

	inputValue := filepath.Join(hwmonPath, strings.TrimPrefix(val.HeaderInput, "/"))

	b, err := os.ReadFile(inputValue)
	if err != nil {
		logger.Log(logger.Fields{"header": header, "path": inputValue, "error": err}).Warn("Unable to read header input value")
		return 0
	}

	s := strings.TrimSpace(string(b))
	n, err := strconv.Atoi(s)
	if err != nil {
		logger.Log(logger.Fields{"header": header, "path": inputValue, "raw": s, "error": err}).Warn("Unable to parse header input value")
		return 0
	}

	if n < 0 {
		return 0
	}
	if n > 32767 {
		return 32767
	}
	return int16(n)
}

// findHwmonByChip scans base with chip and returns full hwmonX path of the given chip
func findHwmonByChip(base, chip string) string {
	base = strings.TrimSpace(base)
	chip = strings.TrimSpace(chip)

	if base == "" || chip == "" {
		logger.Log(logger.Fields{"base": base, "chip": chip}).Warn("base or chip is empty")
		return ""
	}

	entries, err := os.ReadDir(base)
	if err != nil {
		logger.Log(logger.Fields{"base": base, "chip": chip, "error": err}).Warn("read hwmon base failed")
		return ""
	}

	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "hwmon") {
			continue
		}

		namePath := filepath.Join(base, e.Name(), "name")
		b, err := os.ReadFile(namePath)
		if err != nil {
			logger.Log(logger.Fields{"base": base, "chip": chip, "error": err}).Warn("read hwmon path failed")
			continue
		}

		if strings.TrimSpace(string(b)) == chip {
			return filepath.Join(base, e.Name())
		}
	}
	return ""
}

// percentToByte will convert percent into byte value
func percentToByte(p int) uint8 {
	if p < 0 {
		p = 0
	} else if p > 100 {
		p = 100
	}
	return uint8((p*255 + 50) / 100)
}
