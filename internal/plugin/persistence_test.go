package plugin

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestPersistenceUsesExactMountBoundariesAndKeepsUnknown(t *testing.T) {
	mounts := `10 1 0:1 / / rw - overlay overlay rw
11 10 8:1 /docker/volumes/plugins /app/plugins rw - ext4 /dev/sda rw
12 10 0:2 / /run rw - tmpfs tmpfs rw
13 11 0:3 / /app/plugins/temporary rw - tmpfs tmpfs rw
14 10 8:1 /docker/volumes/a /space\040dir rw - xfs /dev/sda rw`
	paths := []persistencePath{{Kind: "billing_database", Path: "/app/plugins/state.db"}, {Kind: "plugin_library", Path: "/app/plugins-other/cpa-key-billing.so"}, {Kind: "turn_state", Path: "/app/plugins/temporary/state.json"}, {Kind: "other", Path: "/space dir/state.db"}, {Kind: "unknown", Path: "relative"}}
	status := inspectPersistence(mounts, paths)
	if !status.Detected || !status.AtRisk || !status.Container || status.CanConfigure {
		t.Fatalf("diagnostic claims=%+v", status)
	}
	want := []string{"mounted", "container_layer", "ephemeral", "mounted", "unknown"}
	for i, item := range status.Paths {
		if item.State != want[i] {
			t.Errorf("%s=%s want %s", item.Path, item.State, want[i])
		}
	}
	for _, raw := range []string{"garbage", `1 0 8:1 / / rw - ext4 /dev/sda rw`} {
		status = inspectPersistence(raw, paths)
		if status.Detected || status.AtRisk {
			t.Fatal("missing or non-container-root mount evidence fabricated persistence")
		}
	}
}

func TestPersistenceResolvesDatabaseAndSidecarIndependently(t *testing.T) {
	dir := t.TempDir()
	database := filepath.Join(dir, "billing.db")
	target := filepath.Join(dir, "another-volume", "sidecar.json")
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, database+".turn-state.json"); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	paths := persistenceStatePaths(database + ".turn-state.json")
	if paths[0].Path != database || paths[1].Path != target {
		t.Fatalf("sidecar symlink changed DB diagnostic: %+v", paths)
	}
	status := inspectPersistence("", []persistencePath{{Kind: "plugin_library", Path: "/gone/cpa-key-billing.so", State: "missing"}})
	if !status.Detected || !status.AtRisk || status.Paths[0].State != "missing" {
		t.Fatal("known missing mapped library incorrectly safe")
	}
}

func TestPersistenceOnlyReturnsThisPluginLibrary(t *testing.T) {
	maps := `aaa-bbb r-xp 0000 08:01 111 /app/plugins/cpa-key-billing.so
bbb-ccc rw-p 0001 08:01 111 /app/plugins/cpa-key-billing.so
ccc-ddd r-xp 0000 08:01 112 /unrelated/secret.so
ddd-eee r-xp 0000 08:01 113 /space dir/cpa-key-billing.so (deleted)
fff-ggg rw-p 0000 00:00 0 [heap]`
	want := []string{"/app/plugins/cpa-key-billing.so", "/space dir/cpa-key-billing.so"}
	if got := loadedPluginPaths(maps); !reflect.DeepEqual(got, want) {
		t.Fatalf("plugin maps=%v", got)
	}
}
