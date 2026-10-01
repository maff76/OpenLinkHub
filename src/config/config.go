package config

// Package: config
// Author: Nikola Jurkovic
// License: GPL-3.0 or later

import (
	"OpenLinkHub/src/common"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"slices"
	"strings"
)

// ByteArray is a byte slice that is stored in config.json as a readable
// array of numbers instead of encoding/json's default base64 string.
// UnmarshalJSON accepts both formats so existing configurations remain compatible.
type ByteArray []byte

func (b ByteArray) MarshalJSON() ([]byte, error) {
	values := make([]uint16, len(b))
	for i, value := range b {
		values[i] = uint16(value)
	}
	return json.Marshal(values)
}

func (b *ByteArray) UnmarshalJSON(data []byte) error {
	var values []uint16
	if err := json.Unmarshal(data, &values); err == nil {
		result := make(ByteArray, len(values))
		for i, value := range values {
			if value > 255 {
				return fmt.Errorf("byte array value %d is outside the valid range 0-255", value)
			}
			result[i] = byte(value)
		}
		*b = result
		return nil
	}

	var encoded string
	if err := json.Unmarshal(data, &encoded); err != nil {
		return fmt.Errorf("byte array must be a numeric array or base64 string: %w", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return fmt.Errorf("invalid base64 byte array: %w", err)
	}
	*b = ByteArray(decoded)
	return nil
}

type Configuration struct {
	Debug                     bool      `json:"debug"`
	ListenPort                int       `json:"listenPort"`
	ListenAddress             string    `json:"listenAddress"`
	CPUSensorChip             string    `json:"cpuSensorChip"`
	Manual                    bool      `json:"manual"`
	Frontend                  bool      `json:"frontend"`
	Metrics                   bool      `json:"metrics"`
	Memory                    bool      `json:"memory"`
	MemorySmBus               string    `json:"memorySmBus"`
	MemoryType                int       `json:"memoryType"`
	Exclude                   []uint16  `json:"exclude"`
	MemorySku                 string    `json:"memorySku"`
	ConfigPath                string    `json:",omitempty"`
	ResumeDelay               int       `json:"resumeDelay"`
	LogFile                   string    `json:"logFile"`
	LogLevel                  string    `json:"logLevel"`
	EnhancementKits           ByteArray `json:"enhancementKits"`
	TemperatureOffset         int       `json:"temperatureOffset"`
	AMDGpuIndex               int       `json:"amdGpuIndex"`
	AMDSmiPath                string    `json:"amdsmiPath"`
	CheckDevicePermission     bool      `json:"checkDevicePermission"`
	GraphProfiles             bool      `json:"graphProfiles"`
	CpuTempFile               string    `json:"cpuTempFile"`
	RamTempViaHwmon           bool      `json:"ramTempViaHwmon"`
	NvidiaGpuIndex            []int     `json:"nvidiaGpuIndex"`
	DefaultNvidiaGPU          int       `json:"defaultNvidiaGPU"`
	OpenRGBPort               int       `json:"openRGBPort"`
	EnableOpenRGBTargetServer bool      `json:"enableOpenRGBTargetServer"`
	EnableGamepad             bool      `json:"enableGamepad"`
	EnableMotherboard         bool      `json:"enableMotherboard"`
	MotherboardBiosOnExit     bool      `json:"motherboardBiosOnExit"`
	MemoryRegisterOverride    ByteArray `json:"memoryRegisterOverride"`
}

var (
	location      = ""
	configuration Configuration
	upgrade       = map[string]any{
		"memorySku":                 "",
		"resumeDelay":               15000,
		"logLevel":                  "info",
		"logFile":                   "",
		"enhancementKits":           make([]byte, 0),
		"temperatureOffset":         0,
		"amdGpuIndex":               0,
		"amdsmiPath":                "",
		"checkDevicePermission":     false,
		"cpuTempFile":               "",
		"graphProfiles":             false,
		"ramTempViaHwmon":           false,
		"nvidiaGpuIndex":            []int{0},
		"defaultNvidiaGPU":          0,
		"openRGBPort":               6743,
		"enableOpenRGBTargetServer": false,
		"enableGamepad":             true,
		"enableMotherboard":         false,
		"motherboardBiosOnExit":     false,
		"memoryRegisterOverride":    make([]byte, 0),
	}
	systemService = true
)

// Init will initialize a new config object
func Init() {
	setSystemService()

	var configPath = ""

	pwd, _ := os.Getwd()
	isAtomic := common.FileExists(pwd + "/atomic")
	if isAtomic {
		pwd = "/etc/OpenLinkHub"
		configPath = "/etc/OpenLinkHub"
	} else {
		configPath = pwd
	}
	location = pwd + "/config.json"

	// Create or upgrade
	upgradeFile(location)

	f, err := os.Open(location)
	if err != nil {
		panic(err.Error())
	}
	if err = json.NewDecoder(f).Decode(&configuration); err != nil {
		panic(err.Error())
	}
	configuration.ConfigPath = configPath
}

// GetConfig will return structs.Configuration struct
func GetConfig() Configuration {
	return configuration
}

// UpdateManual updates the manual fan speed setting and persists it to config.json.
// A service restart is required for device monitoring loops to fully apply the new mode.
func UpdateManual(enabled bool) uint8 {
	configuration.Manual = enabled
	saveConfigSettings(configuration)
	return 1
}

// EditableSettings contains user-facing configuration options that can be safely
// persisted from the Control Panel. These settings are applied after a service restart.
type EditableSettings struct {
	Debug                     bool      `json:"debug"`
	LogLevel                  string    `json:"logLevel"`
	MemorySku                 string    `json:"memorySku"`
	MemoryRegisterOverride    ByteArray `json:"memoryRegisterOverride"`
	GraphProfiles             bool      `json:"graphProfiles"`
	Metrics                   bool      `json:"metrics"`
	RamTempViaHwmon           bool      `json:"ramTempViaHwmon"`
	EnableGamepad             bool      `json:"enableGamepad"`
	EnableMotherboard         bool      `json:"enableMotherboard"`
	MotherboardBiosOnExit     bool      `json:"motherboardBiosOnExit"`
	EnableOpenRGBTargetServer bool      `json:"enableOpenRGBTargetServer"`
	OpenRGBPort               int       `json:"openRGBPort"`
	ResumeDelay               int       `json:"resumeDelay"`
	TemperatureOffset         int       `json:"temperatureOffset"`
	CheckDevicePermission     bool      `json:"checkDevicePermission"`
}

// GetEditableSettings returns the configuration subset exposed by the Control Panel.
func GetEditableSettings() EditableSettings {
	return EditableSettings{
		Debug:                     configuration.Debug,
		LogLevel:                  configuration.LogLevel,
		MemorySku:                 configuration.MemorySku,
		MemoryRegisterOverride:    append(ByteArray(nil), configuration.MemoryRegisterOverride...),
		GraphProfiles:             configuration.GraphProfiles,
		Metrics:                   configuration.Metrics,
		RamTempViaHwmon:           configuration.RamTempViaHwmon,
		EnableGamepad:             configuration.EnableGamepad,
		EnableMotherboard:         configuration.EnableMotherboard,
		MotherboardBiosOnExit:     configuration.MotherboardBiosOnExit,
		EnableOpenRGBTargetServer: configuration.EnableOpenRGBTargetServer,
		OpenRGBPort:               configuration.OpenRGBPort,
		ResumeDelay:               configuration.ResumeDelay,
		TemperatureOffset:         configuration.TemperatureOffset,
		CheckDevicePermission:     configuration.CheckDevicePermission,
	}
}

// UpdateEditableSettings validates and persists the Control Panel configuration.
func UpdateEditableSettings(settings EditableSettings) error {
	validLogLevels := map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
	if !validLogLevels[settings.LogLevel] {
		return fmt.Errorf("log level must be debug, info, warn, or error")
	}
	if len(settings.MemoryRegisterOverride) > 32 {
		return fmt.Errorf("memory register override cannot contain more than 32 addresses")
	}
	if settings.OpenRGBPort < 1 || settings.OpenRGBPort > 65535 {
		return fmt.Errorf("OpenRGB port must be between 1 and 65535")
	}
	if settings.ResumeDelay < 0 || settings.ResumeDelay > 120000 {
		return fmt.Errorf("resume delay must be between 0 and 120000 milliseconds")
	}
	if settings.TemperatureOffset < -50 || settings.TemperatureOffset > 50 {
		return fmt.Errorf("temperature offset must be between -50 and 50 degrees")
	}

	configuration.Debug = settings.Debug
	configuration.LogLevel = settings.LogLevel
	configuration.MemorySku = settings.MemorySku
	configuration.MemoryRegisterOverride = append(ByteArray(nil), settings.MemoryRegisterOverride...)
	configuration.GraphProfiles = settings.GraphProfiles
	configuration.Metrics = settings.Metrics
	configuration.RamTempViaHwmon = settings.RamTempViaHwmon
	configuration.EnableGamepad = settings.EnableGamepad
	configuration.EnableMotherboard = settings.EnableMotherboard
	configuration.MotherboardBiosOnExit = settings.MotherboardBiosOnExit
	configuration.EnableOpenRGBTargetServer = settings.EnableOpenRGBTargetServer
	configuration.OpenRGBPort = settings.OpenRGBPort
	configuration.ResumeDelay = settings.ResumeDelay
	configuration.TemperatureOffset = settings.TemperatureOffset
	configuration.CheckDevicePermission = settings.CheckDevicePermission
	saveConfigSettings(configuration)
	return nil
}

// UpdateSupportedDevices will update the Exclude slice based on the enabled flag for each product ID
func UpdateSupportedDevices(productIds map[uint16]bool) uint8 {
	for productId, enabled := range productIds {
		if enabled {
			if i := slices.Index(configuration.Exclude, productId); i != -1 {
				configuration.Exclude = append(configuration.Exclude[:i], configuration.Exclude[i+1:]...)
			}
		} else {
			if !slices.Contains(configuration.Exclude, productId) {
				configuration.Exclude = append(configuration.Exclude, productId)
			}
		}
	}
	saveConfigSettings(configuration)
	return 1
}

// IsSystemService will return true if service runs under system context
func IsSystemService() bool {
	return systemService
}

// upgradeFile will create or upgrade config file
func upgradeFile(cfg string) {
	if !common.FileExists(cfg) {
		value := &Configuration{
			Debug:                     false,
			ListenPort:                27003,
			ListenAddress:             "127.0.0.1",
			CPUSensorChip:             "",
			Manual:                    false,
			Frontend:                  true,
			Metrics:                   false,
			Memory:                    false,
			MemorySmBus:               "i2c-0",
			MemoryType:                5,
			Exclude:                   make([]uint16, 0),
			MemorySku:                 "",
			ResumeDelay:               15000,
			LogLevel:                  "info",
			LogFile:                   "",
			EnhancementKits:           make(ByteArray, 0),
			TemperatureOffset:         0,
			AMDGpuIndex:               0,
			AMDSmiPath:                "",
			CheckDevicePermission:     false,
			CpuTempFile:               "",
			GraphProfiles:             true,
			RamTempViaHwmon:           true,
			NvidiaGpuIndex:            []int{0},
			DefaultNvidiaGPU:          0,
			OpenRGBPort:               6743,
			EnableOpenRGBTargetServer: false,
			EnableGamepad:             true,
			EnableMotherboard:         false,
			MotherboardBiosOnExit:     false,
			MemoryRegisterOverride:    make(ByteArray, 0),
		}
		saveConfigSettings(value)
	} else {
		save := false
		var data map[string]interface{}
		file, err := os.Open(location)
		defer func(file *os.File) {
			err = file.Close()
			if err != nil {
				panic(err.Error())
			}
		}(file)

		if err != nil {
			panic(err.Error())
		}
		if err = json.NewDecoder(file).Decode(&data); err != nil {
			panic(err.Error())
		}

		// Loop thru upgrade value
		for key, value := range upgrade {
			if _, ok := data[key]; !ok {
				data[key] = value
				save = true
			}
		}
		if save {
			saveConfigSettings(data)
		}
	}
}

// SaveConfigSettings will save dashboard settings
func saveConfigSettings(data any) {
	// Convert to JSON
	buffer, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		panic(err.Error())
	}

	// Create profile filename
	file, err := os.Create(location)
	if err != nil {
		panic(err.Error())
	}

	// Write JSON buffer to file
	_, err = file.Write(buffer)
	if err != nil {
		panic(err.Error())
	}

	// Close file
	err = file.Close()
	if err != nil {
		panic(err.Error())
	}
}

// setSystemService will check and set systemService state
func setSystemService() {
	uid := os.Getuid()
	if uid < 1000 {
		systemService = true
		return
	}

	if os.Getenv("DISPLAY") != "" ||
		os.Getenv("WAYLAND_DISPLAY") != "" ||
		os.Getenv("XDG_SESSION_TYPE") != "" {
		systemService = false
	}

	u, err := user.Current()
	if err != nil {
		systemService = true
		return
	}

	systemService = !strings.HasPrefix(u.HomeDir, "/home/")
}
