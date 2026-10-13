package icueprofile

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ImportedProfile is the compact representation exposed to the UI. The full
// parsed profile remains available by parsing Path when the renderer needs it.
type ImportedProfile struct {
	FileName      string         `json:"fileName"`
	Name          string         `json:"name"`
	ID            string         `json:"id,omitempty"`
	DeviceEntries int            `json:"deviceEntries"`
	Channels      int            `json:"channels"`
	Layers        int            `json:"layers"`
	EnabledLayers int            `json:"enabledLayers"`
	Effects       map[string]int `json:"effects"`
}

func Directory(configPath string) string {
	return filepath.Join(configPath, "database", "rgb", "icue")
}

// EnsureDirectory creates the persistent iCUE profile import directory.
// It is safe to call during startup and before every scan/upload.
func EnsureDirectory(configPath string) (string, error) {
	dir := Directory(configPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return dir, fmt.Errorf("create iCUE profile directory %q: %w", dir, err)
	}
	return dir, nil
}

// Scan parses every imported .cueprofile. Invalid files are reported without
// preventing valid profiles from being returned.
func Scan(configPath string) ([]ImportedProfile, map[string]string, error) {
	dir, err := EnsureDirectory(configPath)
	if err != nil {
		return nil, nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	profiles := make([]ImportedProfile, 0)
	failures := map[string]string{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".cueprofile") {
			continue
		}
		p, err := ParseFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			failures[entry.Name()] = err.Error()
			continue
		}
		profiles = append(profiles, ImportedProfile{
			FileName: entry.Name(), Name: p.Name, ID: p.ID,
			DeviceEntries: p.Summary.DeviceEntries, Channels: p.Summary.Channels,
			Layers: p.Summary.Layers, EnabledLayers: p.Summary.EnabledLayers,
			Effects: p.Summary.Effects,
		})
	}
	sort.Slice(profiles, func(i, j int) bool {
		return strings.ToLower(profiles[i].Name) < strings.ToLower(profiles[j].Name)
	})
	return profiles, failures, nil
}

// Save validates the profile before atomically placing it in the import
// directory. Existing profiles with the same filename are intentionally
// replaced, making re-importing an updated Corsair profile predictable.
func Save(configPath, fileName string, data []byte) (*Profile, string, error) {
	base := filepath.Base(strings.TrimSpace(fileName))
	if base == "." || base == "" || !strings.EqualFold(filepath.Ext(base), ".cueprofile") {
		return nil, "", fmt.Errorf("only .cueprofile files are supported")
	}
	dir, err := EnsureDirectory(configPath)
	if err != nil {
		return nil, "", err
	}
	p, err := Parse(strings.NewReader(string(data)))
	if err != nil {
		return nil, "", fmt.Errorf("parse %s: %w", base, err)
	}
	tmp, err := os.CreateTemp(dir, ".icue-upload-*")
	if err != nil {
		return nil, "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err = tmp.Write(data); err != nil {
		tmp.Close()
		return nil, "", err
	}
	if err = tmp.Close(); err != nil {
		return nil, "", err
	}
	dest := filepath.Join(dir, base)
	if err = os.Rename(tmpName, dest); err != nil {
		return nil, "", err
	}
	return p, dest, nil
}
