package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestEnsureCreatesExpectedConfigWithSafePermissions(t *testing.T) {
	home := t.TempDir()
	manager, err := NewManager(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg, warnings, err := manager.Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cfg != Defaults() {
		t.Fatalf("defaults changed: %#v", cfg)
	}
	if len(warnings) == 0 || !strings.Contains(warnings[len(warnings)-1], FileName) {
		t.Fatalf("expected creation warning, got %v", warnings)
	}
	info, err := os.Stat(manager.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode = %o, want 700", info.Mode().Perm())
	}
	info, err = os.Stat(manager.Path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %o, want 600", info.Mode().Perm())
	}
}

func TestInvalidConfigIsPreserved(t *testing.T) {
	home := t.TempDir()
	manager, err := NewManager(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(manager.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	original := "version: 1\nrefresh_interval: \"not-a-duration\"\n"
	if err := os.WriteFile(manager.Path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := manager.Load(context.Background()); err == nil {
		t.Fatal("expected invalid config error")
	}
	data, err := os.ReadFile(manager.Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != original {
		t.Fatalf("config changed: %q", data)
	}
}

func TestSaveRefusesSymlinkedConfigAndBackup(t *testing.T) {
	home := t.TempDir()
	manager, err := NewManager(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(manager.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(home, "outside")
	if err := os.WriteFile(target, []byte("do not change"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, manager.Path); err != nil {
		t.Fatal(err)
	}
	if err := manager.Save(context.Background(), Defaults()); err == nil {
		t.Fatal("expected config symlink refusal")
	}
	if got, _ := os.ReadFile(target); string(got) != "do not change" {
		t.Fatalf("symlink target changed: %q", got)
	}

	if err := os.Remove(manager.Path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manager.Path, []byte(Defaults().YAML()), 0o600); err != nil {
		t.Fatal(err)
	}
	backupTarget := filepath.Join(home, "backup-outside")
	if err := os.WriteFile(backupTarget, []byte("safe"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(backupTarget, manager.Path+".bak"); err != nil {
		t.Fatal(err)
	}
	if err := manager.Save(context.Background(), Defaults()); err == nil {
		t.Fatal("expected backup symlink refusal")
	}
	if got, _ := os.ReadFile(backupTarget); string(got) != "safe" {
		t.Fatalf("backup symlink target changed: %q", got)
	}
}

func TestConcurrentSavesSerializeAndLeaveValidConfig(t *testing.T) {
	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base := Defaults()
	done := make(chan error, 8)
	for i, sortKey := range []string{"port", "name", "address", "exposure", "port", "name", "address", "exposure"} {
		go func(i int, sortKey string) {
			cfg := base
			cfg.Sort = sortKey
			cfg.OperationTimeout = time.Duration(i+1) * time.Second
			done <- manager.Save(context.Background(), cfg)
		}(i, sortKey)
	}
	for i := 0; i < 8; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	cfg, _, _, err := manager.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestSaveLockAcquisitionHonorsContext(t *testing.T) {
	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(manager.Path+".lock", os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := manager.Save(ctx, Defaults()); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("lock wait was not bounded: %v", err)
	}
}

func TestLoadRejectsOversizedConfigWithoutChangingIt(t *testing.T) {
	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(manager.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	original := strings.Repeat("x", 64*1024+1)
	if err := os.WriteFile(manager.Path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := manager.Load(context.Background()); err == nil {
		t.Fatal("expected oversized config rejection")
	}
	data, err := os.ReadFile(manager.Path)
	if err != nil || string(data) != original {
		t.Fatalf("oversized config was changed: len=%d err=%v", len(data), err)
	}
}

func TestLoadRefusesBroadConfigDirectoryPermissions(t *testing.T) {
	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(manager.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := manager.Load(context.Background()); err == nil || !strings.Contains(err.Error(), "permissions are too broad") {
		t.Fatalf("expected broad directory refusal, got %v", err)
	}
}

func TestSaveRefusesBroadConfigPermissions(t *testing.T) {
	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(manager.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manager.Path, []byte(Defaults().YAML()), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := manager.Save(context.Background(), Defaults()); err == nil {
		t.Fatal("expected unsafe permission refusal")
	}
}

func TestSetValueValidatesBounds(t *testing.T) {
	cfg := Defaults()
	for _, test := range []struct {
		key, value string
	}{
		{"refresh_interval", "0s"},
		{"refresh_interval", "25h"},
		{"operation_timeout", "500ms"},
		{"sort", "random"},
		{"color_theme", "neon"},
		{"show_system_listeners", "maybe"},
	} {
		if err := SetValue(&cfg, test.key, test.value); err == nil {
			t.Errorf("SetValue(%q, %q) accepted unsafe value", test.key, test.value)
		}
	}
	if err := SetValue(&cfg, "refresh_interval", "250ms"); err != nil {
		t.Fatal(err)
	}
	if cfg.RefreshInterval != 250*time.Millisecond {
		t.Fatalf("refresh interval = %s", cfg.RefreshInterval)
	}
}
