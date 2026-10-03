package memory

// Package: memory
// File: eepromDecoder.go
// Description: This file provides the basic framework and functionality to decode EEPROM data but extracts only the SKU/Part Number from the EEPROM data of DDR5 memory modules.
// Author: PabloGS
// License: GPL-3.0 or later

import (
	"OpenLinkHub/src/logger"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
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
