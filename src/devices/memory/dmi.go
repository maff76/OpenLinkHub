package memory

import (
	"OpenLinkHub/src/logger"
	"os/exec"
	"strconv"
	"strings"
)

// DIMIMemoryDevice contains SMBIOS Type 17 data reported by system firmware.
// It is enrichment only: SMBus/SPD/hwmon remain authoritative for discovery and telemetry.
type DIMIMemoryDevice struct {
	Locator               string
	BankLocator           string
	Size                  string
	FormFactor            string
	Type                  string
	TypeDetail            string
	Speed                 int
	Manufacturer          string
	SerialNumber          string
	PartNumber            string
	Rank                  int
	ConfiguredMemorySpeed int
	MinimumVoltage        float64
	MaximumVoltage        float64
	ConfiguredVoltage     float64
}

func parseDMIInt(value string) int {
	fields := strings.Fields(strings.TrimSpace(value))
	if len(fields) == 0 {
		return 0
	}
	v, _ := strconv.Atoi(fields[0])
	return v
}

func parseDMIVoltage(value string) float64 {
	fields := strings.Fields(strings.TrimSpace(value))
	if len(fields) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(fields[0], 64)
	return v
}

// getDIMIMemoryDevices reads populated SMBIOS Type 17 entries through dmidecode.
// dmidecode is deliberately optional: failure never prevents memory discovery.
func getDIMIMemoryDevices() []DIMIMemoryDevice {
	path, err := exec.LookPath("dmidecode")
	if err != nil {
		logger.Log(logger.Fields{"error": err}).Warn("dmidecode not available; memory SMBIOS metadata disabled")
		return nil
	}
	output, err := exec.Command(path, "--type", "17").Output()
	if err != nil {
		logger.Log(logger.Fields{"error": err}).Warn("Unable to read SMBIOS memory metadata")
		return nil
	}

	var devices []DIMIMemoryDevice
	var current *DIMIMemoryDevice
	flush := func() {
		if current == nil {
			return
		}
		size := strings.TrimSpace(current.Size)
		if size != "" && !strings.EqualFold(size, "No Module Installed") && !strings.EqualFold(size, "Unknown") && size != "0 MB" {
			devices = append(devices, *current)
		}
		current = nil
	}

	for _, raw := range strings.Split(string(output), "\n") {
		line := strings.TrimSpace(raw)
		if line == "Memory Device" {
			flush()
			current = &DIMIMemoryDevice{}
			continue
		}
		if current == nil || !strings.Contains(line, ":") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		key, value := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		switch key {
		case "Size":
			current.Size = value
		case "Form Factor":
			current.FormFactor = value
		case "Locator":
			current.Locator = value
		case "Bank Locator":
			current.BankLocator = value
		case "Type":
			current.Type = value
		case "Type Detail":
			current.TypeDetail = value
		case "Speed":
			current.Speed = parseDMIInt(value)
		case "Manufacturer":
			current.Manufacturer = value
		case "Serial Number":
			current.SerialNumber = value
		case "Part Number":
			current.PartNumber = strings.TrimSpace(value)
		case "Rank":
			current.Rank = parseDMIInt(value)
		case "Configured Memory Speed":
			current.ConfiguredMemorySpeed = parseDMIInt(value)
		case "Minimum Voltage":
			current.MinimumVoltage = parseDMIVoltage(value)
		case "Maximum Voltage":
			current.MaximumVoltage = parseDMIVoltage(value)
		case "Configured Voltage":
			current.ConfiguredVoltage = parseDMIVoltage(value)
		}
	}
	flush()
	return devices
}
