package plugin

import (
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

type persistencePath struct {
	Kind  string `json:"kind"`
	Path  string `json:"path"`
	State string `json:"state"`
}

type persistenceStatus struct {
	Detected     bool              `json:"detected"`
	Container    bool              `json:"container"`
	AtRisk       bool              `json:"at_risk"`
	CanConfigure bool              `json:"can_configure"`
	Paths        []persistencePath `json:"paths"`
}

type persistenceMount struct{ point, fs string }

// This is a storage diagnostic, not an autoload guarantee. The host supplies
// neither a container administration callback nor deployment configuration.
// A root overlay proven inside a container is not external storage: changes
// made there may be lost on container replacement (image-baked files can
// survive). A separate filesystem mount is evidence of external storage only.
// Other deployments remain unknown.
func (a *App) getPersistence(_ ManagementRequest) ManagementResponse {
	result := persistenceStatus{Paths: []persistencePath{}}
	if runtime.GOOS == "linux" {
		_, dockerErr := os.Stat("/.dockerenv")
		_, podmanErr := os.Stat("/run/.containerenv")
		if dockerErr == nil || podmanErr == nil {
			mounts, mountErr := os.ReadFile("/proc/self/mountinfo")
			maps, _ := os.ReadFile("/proc/self/maps")
			if mountErr == nil {
				paths := persistenceStatePaths(a.turnState.StoragePath())
				libraries := loadedPluginPaths(string(maps))
				if len(libraries) == 0 {
					paths = append(paths, persistencePath{Kind: "plugin_library"})
				}
				for _, library := range libraries {
					item := persistencePath{Kind: "plugin_library", Path: resolvePersistencePath(library)}
					if _, err := os.Stat(library); os.IsNotExist(err) {
						item.State = "missing"
					}
					paths = append(paths, item)
				}
				result = inspectPersistence(string(mounts), paths)
			}
		}
	}
	response := JSONResponse(http.StatusOK, result)
	response.Headers.Set("Cache-Control", "private, no-store")
	return response
}

func persistenceStatePaths(rawStatePath string) []persistencePath {
	// Resolve each configured file independently: a sidecar symlink may point
	// to a different volume than the billing database beside its link.
	return []persistencePath{
		{Kind: "billing_database", Path: resolvePersistencePath(strings.TrimSuffix(rawStatePath, ".turn-state.json"))},
		{Kind: "turn_state", Path: resolvePersistencePath(rawStatePath)},
		{Kind: "turn_state_runtime", Path: resolvePersistencePath(rawStatePath + ".runtime.json")},
		{Kind: "turn_state_runner", Path: resolvePersistencePath(strings.TrimSuffix(rawStatePath, ".turn-state.json") + ".turn-state-runner.json")},
		{Kind: "account_runtime", Path: resolvePersistencePath(strings.TrimSuffix(rawStatePath, ".turn-state.json") + ".account-runtime.json")},
		{Kind: "risk_control", Path: resolvePersistencePath(strings.TrimSuffix(rawStatePath, ".turn-state.json") + ".risk-control.json")},
	}
}

// Resolve symlinks before matching mounts, including not-yet-created sidecars.
// If resolution is not possible the diagnostic leaves that path unknown.
func resolvePersistencePath(value string) string {
	if value == "" {
		return ""
	}
	abs, err := filepath.Abs(value)
	if err != nil {
		return ""
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err == nil {
		return resolved
	}
	if !os.IsNotExist(err) {
		return ""
	}
	parent := filepath.Dir(abs)
	if parent == abs {
		return ""
	}
	resolved = resolvePersistencePath(parent)
	if resolved == "" {
		return ""
	}
	return filepath.Join(resolved, filepath.Base(abs))
}

func loadedPluginPaths(maps string) []string {
	seen := map[string]bool{}
	for _, line := range strings.Split(maps, "\n") {
		start := strings.IndexByte(line, '/')
		if start < 0 {
			continue
		}
		file := strings.TrimSuffix(strings.TrimSpace(line[start:]), " (deleted)")
		if path.Base(file) == PluginID+".so" {
			seen[file] = true
		}
	}
	result := make([]string, 0, len(seen))
	for file := range seen {
		result = append(result, file)
	}
	sort.Strings(result)
	return result
}

func inspectPersistence(mountinfo string, paths []persistencePath) persistenceStatus {
	result := persistenceStatus{Container: true, Paths: []persistencePath{}}
	mounts := []persistenceMount{}
	unescape := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	for _, line := range strings.Split(mountinfo, "\n") {
		parts := strings.SplitN(line, " - ", 2)
		if len(parts) != 2 {
			continue
		}
		left, right := strings.Fields(parts[0]), strings.Fields(parts[1])
		if len(left) < 6 || len(right) < 3 {
			continue
		}
		mounts = append(mounts, persistenceMount{point: unescape.Replace(left[4]), fs: right[0]})
	}
	for _, item := range paths {
		if item.State == "missing" && item.Kind == "plugin_library" {
			result.Detected, result.AtRisk = true, true
			result.Paths = append(result.Paths, item)
			continue
		}
		item.State = "unknown"
		best := persistenceMount{}
		if path.IsAbs(item.Path) {
			for _, mount := range mounts {
				if mount.point == "/" || item.Path == mount.point || strings.HasPrefix(item.Path, mount.point+"/") {
					if len(mount.point) > len(best.point) {
						best = mount
					}
				}
			}
		}
		switch best.fs {
		case "tmpfs", "ramfs":
			item.State = "ephemeral"
		case "overlay", "fuse.overlayfs":
			if best.point == "/" {
				item.State = "container_layer"
			}
		case "ext2", "ext3", "ext4", "xfs", "btrfs", "zfs", "nfs", "nfs4", "cifs", "9p", "virtiofs":
			if best.point != "/" && best.point != "" {
				item.State = "mounted"
			}
		}
		if item.State != "unknown" {
			result.Detected = true
		}
		if item.State == "ephemeral" || item.State == "container_layer" {
			result.AtRisk = true
		}
		result.Paths = append(result.Paths, item)
	}
	return result
}
