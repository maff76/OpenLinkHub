package memory

// Package: memory
// File: eepromDecoder.go
// Description: This file provides the basic framework and functionality to decode EEPROM data but extracts only the SKU/Part Number from the EEPROM data of DDR5 memory modules.
// Author: PabloGS
// License: GPL-3.0 or later

import (
	"OpenLinkHub/src/logger"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	hwmonRoot = "/sys/class/hwmon"
)

// RAMModule holds the decoded information from the EEPROM data of a RAM module.
// The struct can be extended to include more attributes as needed.
type RAMModule struct {
	// Hardware metadata
	EEPROMPath string // Path to the EEPROM within hwmon device directory
	SPDAddress string // I2C SPD address, e.g. 0x50
	SKU        string // SKU is the part number or identifier for the RAM module

	// Optional rich DDR5 SPD metadata. These fields are populated through the
	// read-only spdr decoder when it is installed. The existing native SKU
	// decoder remains the fallback, so memory discovery never depends on spdr.
	SPDRevision        string
	SPDCRCValid        bool
	DRAMDeviceType     string
	ModuleType         string
	DensityPerDie      string
	Package            string
	DiesPerPackage     int
	IOWidth            string
	BankGroups         int
	BanksPerBankGroup  int
	RanksPerChannel    int
	RankMix            string
	ChannelsPerDIMM    int
	BusWidthPerChannel int
	JEDECDataRate      int
	ModuleHeightMM     int
	DRAMManufacturer   string
	ManufacturingDate  string
	DRAMStepping       int
	XMPPresent         bool
	XMPProfileName     string
	XMPDataRate        int
	XMPCASLatency      int
	XMPtrCD            int
	XMPtrP             int
	XMPtrAS            int
	XMPVDD             float64
	XMPVDDQ            float64
	XMPVPP             float64
	XMPCRCValid        bool
	EXPOPresent        bool
}

// parseSKUInfo Reads the byte range the SKU/Part Number is normally found at and filters out non-printable ASCII characters
func parseSKUInfo(m *RAMModule, spd []byte) {
	if len(spd) >= 0x021B {
		partBytes := spd[0x0209:0x021B]
		for _, b := range partBytes {
			if b >= 32 && b <= 126 {
				m.SKU += string(b)
			}
		}
	}

}

func valueAfterColon(line string) string {
	parts := strings.SplitN(line, ":", 2)
	if len(parts) != 2 {
		return ""
	}
	return strings.TrimSpace(parts[1])
}

func leadingInt(value string) int {
	fields := strings.Fields(value)
	if len(fields) == 0 {
		return 0
	}
	v, _ := strconv.Atoi(strings.TrimPrefix(fields[0], "CL"))
	return v
}

func leadingFloat(value string) float64 {
	fields := strings.Fields(value)
	if len(fields) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(fields[0], 64)
	return v
}

func dataRate(value string) int {
	start := strings.Index(value, "DDR5-")
	if start < 0 {
		return 0
	}
	start += len("DDR5-")
	end := start
	for end < len(value) && value[end] >= '0' && value[end] <= '9' {
		end++
	}
	v, _ := strconv.Atoi(value[start:end])
	return v
}

// enrichWithSPDR uses the optional read-only spdr CLI to decode richer DDR5
// metadata. Failure is deliberately silent: SPD discovery and the native part
// number decoder must continue to work on systems where spdr is not installed.
func enrichWithSPDR(m *RAMModule, path string) {
	binary, err := exec.LookPath("spdr")
	if err != nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, binary, "decode", path).Output()
	if err != nil {
		return
	}

	section := ""
	xmpProfile := false
	for _, raw := range strings.Split(string(out), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = line
			xmpProfile = false
			continue
		}
		value := valueAfterColon(line)
		switch section {
		case "[Identity and base]":
			switch {
			case strings.HasPrefix(line, "SPD revision:"):
				m.SPDRevision = value
			case strings.HasPrefix(line, "DRAM device type:"):
				m.DRAMDeviceType = value
			case strings.HasPrefix(line, "Module type:"):
				m.ModuleType = value
			case strings.HasPrefix(line, "Density per die:"):
				m.DensityPerDie = value
			case strings.HasPrefix(line, "Package:"):
				m.Package = value
			case strings.HasPrefix(line, "Dies per package:"):
				m.DiesPerPackage = leadingInt(value)
			case strings.HasPrefix(line, "I/O width:"):
				m.IOWidth = value
			case strings.HasPrefix(line, "Bank groups:"):
				m.BankGroups = leadingInt(value)
			case strings.HasPrefix(line, "Banks per bank group:"):
				m.BanksPerBankGroup = leadingInt(value)
			case strings.HasPrefix(line, "Package ranks per channel:"):
				m.RanksPerChannel = leadingInt(value)
			case strings.HasPrefix(line, "Rank mix:"):
				m.RankMix = value
			case strings.HasPrefix(line, "Channels per DIMM:"):
				m.ChannelsPerDIMM = leadingInt(value)
			case strings.HasPrefix(line, "Primary bus width per channel:"):
				m.BusWidthPerChannel = leadingInt(value)
			}
		case "[Base configuration CRC]":
			if strings.HasPrefix(line, "Match:") {
				m.SPDCRCValid = strings.EqualFold(value, "yes")
			}
		case "[JEDEC base timings]":
			if strings.HasPrefix(line, "Base data rate:") {
				m.JEDECDataRate = dataRate(value)
			}
		case "[Module-specific]":
			if strings.HasPrefix(line, "Nominal height:") {
				m.ModuleHeightMM = leadingInt(value)
			}
		case "[Manufacturing]":
			switch {
			case strings.HasPrefix(line, "DRAM manufacturer:"):
				m.DRAMManufacturer = value
			case strings.HasPrefix(line, "Manufacturing date:"):
				m.ManufacturingDate = value
			case strings.HasPrefix(line, "DRAM stepping:"):
				m.DRAMStepping = leadingInt(value)
			}
		case "[Vendor profiles (XMP 3.0 / EXPO)]":
			switch {
			case strings.HasPrefix(line, "Intel XMP 3.0:"):
				m.XMPPresent = strings.EqualFold(value, "present")
				xmpProfile = false
			case strings.HasPrefix(line, "Profile 1:"):
				if m.XMPPresent && value != "(not enabled)" {
					xmpProfile = true
					m.XMPProfileName = value
				}
			case strings.HasPrefix(line, "Profile 2:"):
				xmpProfile = false
			case strings.HasPrefix(line, "AMD EXPO:"):
				m.EXPOPresent = strings.EqualFold(value, "present")
				xmpProfile = false
			case xmpProfile && strings.HasPrefix(line, "Data rate:"):
				m.XMPDataRate = dataRate(value)
			case xmpProfile && strings.HasPrefix(line, "CAS latency:"):
				m.XMPCASLatency = leadingInt(value)
			case xmpProfile && strings.HasPrefix(line, "tRCD:"):
				if i := strings.Index(value, "("); i >= 0 {
					m.XMPtrCD = leadingInt(value[i+1:])
				}
			case xmpProfile && strings.HasPrefix(line, "tRP:"):
				if i := strings.Index(value, "("); i >= 0 {
					m.XMPtrP = leadingInt(value[i+1:])
				}
			case xmpProfile && strings.HasPrefix(line, "tRAS:"):
				if i := strings.Index(value, "("); i >= 0 {
					m.XMPtrAS = leadingInt(value[i+1:])
				}
			case xmpProfile && strings.HasPrefix(line, "VDD / VDDQ / VPP:"):
				parts := strings.Split(value, "/")
				if len(parts) == 3 {
					m.XMPVDD = leadingFloat(strings.TrimSpace(parts[0]))
					m.XMPVDDQ = leadingFloat(strings.TrimSpace(parts[1]))
					m.XMPVPP = leadingFloat(strings.TrimSpace(parts[2]))
				}
			case xmpProfile && strings.HasPrefix(line, "Section CRC:"):
				m.XMPCRCValid = strings.Contains(value, "(match)")
			}
		}
	}
}

func i2cAddressFromPath(path string) string {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		path = resolved
	}
	for _, part := range strings.Split(filepath.Clean(path), string(os.PathSeparator)) {
		bits := strings.Split(part, "-")
		if len(bits) != 2 || len(bits[1]) != 4 {
			continue
		}
		value, err := strconv.ParseUint(bits[1], 16, 8)
		if err == nil {
			return fmt.Sprintf("0x%02X", value)
		}
	}
	return ""
}

// parseSPDModule Parse the SPD data from the EEPROM file.
func parseSPDModule(path string, spd []byte) RAMModule {
	var m RAMModule
	m.EEPROMPath = path
	m.SPDAddress = i2cAddressFromPath(path)
	parseSKUInfo(&m, spd)
	enrichWithSPDR(&m, path)
	return m
}

// findEEPROMs traverse the hwmon directory structure to find EEPROMs
func findEEPROMs(i2cBus int) ([]string, error) {
	var paths []string

	entries, err := os.ReadDir(hwmonRoot)
	if err != nil {
		return nil, err
	}

	for _, entry := range entries {
		namePath := filepath.Join(hwmonRoot, entry.Name(), "name")
		nameBytes, err := os.ReadFile(namePath)
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(nameBytes)) == "spd5118" {
			eepromPath := filepath.Join(hwmonRoot, entry.Name(), "device", "eeprom")
			resolvedPath, err := filepath.EvalSymlinks(eepromPath)
			if err != nil {
				continue
			}
			busPrefix := fmt.Sprintf("%d-", i2cBus)
			onBus := false
			for _, part := range strings.Split(filepath.Clean(resolvedPath), string(os.PathSeparator)) {
				if strings.HasPrefix(part, busPrefix) {
					onBus = true
					break
				}
			}
			if onBus {
				paths = append(paths, eepromPath)
			}
		}
	}

	if len(paths) == 0 {
		return nil, fmt.Errorf("no EEPROMs found")
	}
	return paths, nil
}

// decodeEEPROMs reads the EEPROM files and decodes the SPD data into RAMModule structs.
func decodeEEPROMs(paths []string) []RAMModule {
	var modules []RAMModule
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			logger.Log(logger.Fields{"error": err, "path": path}).Error("Failed to read eeprom data")
			continue
		}
		module := parseSPDModule(path, data)
		modules = append(modules, module)
	}
	return modules
}

// NewMemoryModules finds and decodes all memory modules in the system.
func NewMemoryModules(i2cBus int) []RAMModule {
	paths, err := findEEPROMs(i2cBus)
	if err != nil {
		// If no EEPROMs are found, return an empty slice and the error
		return nil
	}

	modules := decodeEEPROMs(paths)
	// hwmon numbers are assigned dynamically and must not define DIMM order.
	// Keep DDR5 modules in physical SPD/I2C address order (0x50, 0x51, ...)
	// so memory.go associates metadata and temperatures deterministically.
	sort.SliceStable(modules, func(i, j int) bool {
		if modules[i].SPDAddress == modules[j].SPDAddress {
			return modules[i].EEPROMPath < modules[j].EEPROMPath
		}
		if modules[i].SPDAddress == "" {
			return false
		}
		if modules[j].SPDAddress == "" {
			return true
		}
		return modules[i].SPDAddress < modules[j].SPDAddress
	})
	return modules
}

/*
// Print decoded SPD information to console. Not intended for production use, but useful for debugging.
func PrintModuleSPDInfo(m RAMModule) {
	fmt.Println("EEPROM Path:         ", m.EEPROMPath)
	fmt.Println("SKU:                 ", m.SKU)
}

// Iterates over all memory modules found in the system and prints their SPD information.
func PrintAllModules(modules []RAMModule) {
	for _, m := range modules {
		fmt.Println("--------------------------------------------------")
		fmt.Println("Memory Module SPD Information:")
		fmt.Println("--------------------------------------------------")
		PrintModuleSPDInfo(m)
	}
	fmt.Println("--------------------------------------------------")
}
*/
