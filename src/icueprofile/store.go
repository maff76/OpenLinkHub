package icueprofile

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
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
	Compatibility Compatibility  `json:"compatibility"`
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
			Effects: p.Summary.Effects, Compatibility: BuildCompatibility(p.Summary.Effects),
		})
	}
	sort.Slice(profiles, func(i, j int) bool {
		return strings.ToLower(profiles[i].Name) < strings.ToLower(profiles[j].Name)
	})
	return profiles, failures, nil
}

type cachedProfile struct {
	profile *Profile
	path    string
	modTime time.Time
	size    int64
}

var profileCache = struct {
	sync.RWMutex
	byKey map[string]cachedProfile
}{byKey: make(map[string]cachedProfile)}

func cacheKey(configPath, key string) string {
	return filepath.Clean(configPath) + "\x00" + key
}

func invalidateProfileCache(configPath, key string) {
	profileCache.Lock()
	delete(profileCache.byKey, cacheKey(configPath, key))
	profileCache.Unlock()
}

// ResetRuntimeProfile discards all in-memory edits for one imported profile.
// The source .cueprofile remains untouched; the next load reparses the authored
// file and playback recompiles from that clean model.
func ResetRuntimeProfile(configPath, key string) (*Profile, string, error) {
	invalidateProfileCache(configPath, key)
	resetRuntimeRenderState()
	return LoadByKey(configPath, key)
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
	invalidateProfileCache(configPath, ProfileKey(base))
	return p, dest, nil
}

// ProfileKey returns the stable OpenLinkHub RGB mode used for an imported
// profile. Keeping the filename in the key makes replacement uploads retain
// the same saved device selection.
func ProfileKey(fileName string) string {
	base := strings.TrimSuffix(filepath.Base(fileName), filepath.Ext(fileName))
	base = strings.ToLower(base)
	var b strings.Builder
	lastDash := false
	for _, r := range base {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
		} else if !lastDash && b.Len() > 0 {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return "icue-" + strings.Trim(b.String(), "-")
}

// LoadByKey resolves an imported profile by its RGB mode key. Parsed profiles
// are cached because this function is on the live RGB frame path: LINK calls
// generateRgbEffect for every device every ~20 ms. Re-parsing the XML for every
// fan on every frame can turn a nominal 50 Hz loop into ~1 Hz.
//
// A cheap Stat keeps direct on-disk replacements working: if size or mtime
// changes, the profile is parsed again. Save also invalidates immediately.
func LoadByKey(configPath, key string) (*Profile, string, error) {
	ck := cacheKey(configPath, key)
	profileCache.RLock()
	cached, ok := profileCache.byKey[ck]
	profileCache.RUnlock()
	if ok {
		if info, err := os.Stat(cached.path); err == nil && info.Size() == cached.size && info.ModTime().Equal(cached.modTime) {
			return cached.profile, cached.path, nil
		}
	}

	dir, err := EnsureDirectory(configPath)
	if err != nil {
		return nil, "", err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, "", err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".cueprofile") {
			continue
		}
		if ProfileKey(entry.Name()) != key {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		p, err := ParseFile(path)
		if err != nil {
			return p, path, err
		}
		info, statErr := os.Stat(path)
		if statErr == nil {
			profileCache.Lock()
			profileCache.byKey[ck] = cachedProfile{profile: p, path: path, modTime: info.ModTime(), size: info.Size()}
			profileCache.Unlock()
		}
		return p, path, nil
	}
	return nil, "", fmt.Errorf("imported iCUE profile %q not found", key)
}

// Modes returns imported profile mode keys and display names.
func Modes(configPath string) map[string]string {
	out := map[string]string{}
	profiles, _, err := Scan(configPath)
	if err != nil {
		return out
	}
	for _, p := range profiles {
		name := strings.TrimSpace(p.Name)
		if name == "" {
			name = strings.TrimSuffix(p.FileName, filepath.Ext(p.FileName))
		}
		out[ProfileKey(p.FileName)] = "iCUE: " + name
	}
	return out
}
