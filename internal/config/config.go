package config

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/arrokh/tailge/internal/model"
)

const (
	DirName        = ".tailge"
	FileName       = "config.tailge"
	maxConfigBytes = 64 * 1024
)

type Config struct {
	Version                     int           `json:"version"`
	RefreshInterval             time.Duration `json:"refresh_interval"`
	Sort                        string        `json:"sort"`
	ColorTheme                  string        `json:"color_theme"`
	ShowSystemListeners         bool          `json:"show_system_listeners"`
	ShowInactiveConfiguredPorts bool          `json:"show_inactive_configured_ports"`
	OperationTimeout            time.Duration `json:"operation_timeout"`
	ServeProbeVersion           string        `json:"serve_probe_version,omitempty"`
	FunnelProbeVersion          string        `json:"funnel_probe_version,omitempty"`
	ServeProbeAt                string        `json:"serve_probe_at,omitempty"`
	FunnelProbeAt               string        `json:"funnel_probe_at,omitempty"`
}

func Defaults() Config {
	return Config{Version: 1, RefreshInterval: 5 * time.Second, Sort: "port", ColorTheme: "auto", ShowSystemListeners: true, ShowInactiveConfiguredPorts: true, OperationTimeout: 15 * time.Second}
}

func (c Config) Validate() error {
	if c.Version != 1 {
		return fmt.Errorf("unsupported config version %d; supported version is 1", c.Version)
	}
	if c.RefreshInterval < 100*time.Millisecond || c.RefreshInterval > 24*time.Hour {
		return fmt.Errorf("refresh_interval must be between 100ms and 24h")
	}
	if c.OperationTimeout < time.Second || c.OperationTimeout > 10*time.Minute {
		return fmt.Errorf("operation_timeout must be between 1s and 10m")
	}
	switch c.Sort {
	case "port", "name", "address", "exposure":
	default:
		return fmt.Errorf("sort must be one of port, name, address, exposure")
	}
	switch c.ColorTheme {
	case "auto", "dark", "light":
	default:
		return fmt.Errorf("color_theme must be one of auto, dark, light")
	}
	return nil
}

func (c Config) YAML() string {
	text := fmt.Sprintf("version: %d\nrefresh_interval: %q\nsort: %s\ncolor_theme: %s\nshow_system_listeners: %t\nshow_inactive_configured_ports: %t\noperation_timeout: %q\n", c.Version, c.RefreshInterval.String(), c.Sort, c.ColorTheme, c.ShowSystemListeners, c.ShowInactiveConfiguredPorts, c.OperationTimeout.String())
	if c.ServeProbeVersion != "" {
		text += fmt.Sprintf("serve_probe_version: %q\nserve_probe_at: %q\n", c.ServeProbeVersion, c.ServeProbeAt)
	}
	if c.FunnelProbeVersion != "" {
		text += fmt.Sprintf("funnel_probe_version: %q\nfunnel_probe_at: %q\n", c.FunnelProbeVersion, c.FunnelProbeAt)
	}
	return text
}

type Manager struct {
	HomeDir string
	Path    string
	Dir     string
}

func NewManager(home string) (Manager, error) {
	if strings.TrimSpace(home) == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return Manager{}, model.WrapError(model.ErrConfig, "config", "cannot determine the home directory", false, "unavailable", "Set a valid home directory and retry.", err)
		}
	}
	home, err := filepath.Abs(home)
	if err != nil {
		return Manager{}, model.WrapError(model.ErrConfig, "config", "cannot resolve the home directory", false, "unavailable", "Use a valid home directory and retry.", err)
	}
	dir := filepath.Join(home, DirName)
	return Manager{HomeDir: home, Dir: dir, Path: filepath.Join(dir, FileName)}, nil
}

func (m Manager) Load(ctx context.Context) (Config, []string, bool, error) {
	if err := ctx.Err(); err != nil {
		return Config{}, nil, false, model.WrapError(model.ErrCancelled, "config", "config load cancelled", true, "cancelled", "Retry the command.", err)
	}
	if err := m.validateParent(); err != nil {
		return Config{}, nil, false, err
	}
	info, err := os.Lstat(m.Path)
	if errors.Is(err, os.ErrNotExist) {
		return Defaults(), []string{"config file is missing; defaults are in use"}, true, nil
	}
	if err != nil {
		return Config{}, nil, false, model.WrapError(model.ErrConfig, "config", "cannot inspect "+m.Path, false, "unavailable", "Check the path permissions and retry.", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return Config{}, nil, false, model.NewError(model.ErrConfig, "config", "config file is an unexpected symlink: "+m.Path, false, "unsafe", "Replace the symlink with a regular file and retry.")
	}
	if !info.Mode().IsRegular() {
		return Config{}, nil, false, model.NewError(model.ErrConfig, "config", "config path is not a regular file: "+m.Path, false, "unsafe", "Replace it with a regular file and retry.")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return Config{}, nil, false, model.NewError(model.ErrConfig, "config", "config file permissions are too broad: "+m.Path, false, "unsafe", "Restrict the file to owner read/write permissions (0600) and retry.")
	}
	data, err := readBounded(m.Path)
	if err != nil {
		return Config{}, nil, false, model.WrapError(model.ErrConfig, "config", "cannot read "+m.Path, false, "unavailable", "Check the file permissions and retry.", err)
	}
	cfg, warnings, err := Parse(string(data))
	if err != nil {
		return Config{}, warnings, false, model.WrapError(model.ErrConfig, "config", "invalid "+m.Path+": "+err.Error(), false, "invalid", "Fix the reported field and run `tailge config validate`; the file was not changed.", err)
	}
	return cfg, warnings, false, nil
}

func (m Manager) Ensure(ctx context.Context) (Config, []string, error) {
	cfg, warnings, missing, err := m.Load(ctx)
	if err != nil {
		return Config{}, warnings, err
	}
	if missing {
		if err := m.Save(ctx, cfg); err != nil {
			warnings = append(warnings, "config could not be persisted; in-memory defaults remain active")
			return cfg, warnings, err
		}
		warnings = append(warnings, "created "+m.Path)
	}
	return cfg, warnings, nil
}

func (m Manager) Save(ctx context.Context, cfg Config) error {
	if err := ctx.Err(); err != nil {
		return model.WrapError(model.ErrCancelled, "config", "config write cancelled", true, "cancelled", "Retry the command.", err)
	}
	if err := cfg.Validate(); err != nil {
		return model.WrapError(model.ErrConfig, "config", err.Error(), false, "invalid", "Correct the value and retry.", err)
	}
	if err := m.ensureDir(); err != nil {
		return err
	}
	lock, unlock, err := m.openWriteLock(ctx)
	if err != nil {
		return err
	}
	defer lock.Close()
	defer unlock()
	return m.saveLocked(ctx, cfg)
}

func (m Manager) saveLocked(ctx context.Context, cfg Config) error {
	if err := ctx.Err(); err != nil {
		return model.WrapError(model.ErrCancelled, "config", "config write cancelled", true, "cancelled", "Retry the command.", err)
	}
	if info, statErr := os.Lstat(m.Path); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return model.NewError(model.ErrConfig, "config", "refusing to replace unsafe config path "+m.Path, false, "unsafe", "Replace the path with a regular file and retry.")
		}
		if info.Mode().Perm()&0o077 != 0 {
			return model.NewError(model.ErrConfig, "config", "config file permissions are too broad: "+m.Path, false, "unsafe", "Restrict the file to owner read/write permissions (0600) and retry.")
		}
		backup := m.Path + ".bak"
		if backupInfo, backupErr := os.Lstat(backup); backupErr == nil {
			if backupInfo.Mode()&os.ModeSymlink != 0 || !backupInfo.Mode().IsRegular() {
				return model.NewError(model.ErrConfig, "config", "refusing unsafe config backup path "+backup, false, "unsafe", "Replace the backup path with a regular file and retry.")
			}
		} else if !errors.Is(backupErr, os.ErrNotExist) {
			return model.WrapError(model.ErrConfig, "config", "cannot inspect config backup", true, "unavailable", "Check permissions and retry.", backupErr)
		}
		if err := copyFile(backup, m.Path, 0o600); err != nil {
			return model.WrapError(model.ErrConfig, "config", "cannot create config backup", true, "unavailable", "Free disk space or fix permissions, then retry.", err)
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return model.WrapError(model.ErrConfig, "config", "cannot inspect config before writing", true, "unavailable", "Check permissions and retry.", statErr)
	}

	tmp, err := os.CreateTemp(m.Dir, ".config.tailge.tmp-")
	if err != nil {
		return model.WrapError(model.ErrConfig, "config", "cannot create temporary config", true, "unavailable", "Check directory permissions and disk space, then retry.", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return model.WrapError(model.ErrConfig, "config", "cannot restrict temporary config permissions", false, "unsafe", "Fix directory permissions and retry.", err)
	}
	if _, err := io.WriteString(tmp, cfg.YAML()); err != nil {
		tmp.Close()
		return model.WrapError(model.ErrConfig, "config", "cannot write config", true, "unavailable", "Free disk space and retry.", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return model.WrapError(model.ErrConfig, "config", "cannot sync config", true, "unavailable", "Check storage health and retry.", err)
	}
	if err := tmp.Close(); err != nil {
		return model.WrapError(model.ErrConfig, "config", "cannot close temporary config", true, "unavailable", "Retry after checking the filesystem.", err)
	}
	if err := os.Rename(tmpName, m.Path); err != nil {
		return model.WrapError(model.ErrConfig, "config", "cannot atomically replace config", true, "unavailable", "Check directory permissions and retry.", err)
	}
	if dir, err := os.Open(m.Dir); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	written, warnings, _, err := m.Load(ctx)
	if err != nil {
		return model.WrapError(model.ErrConfig, "config", "config write could not be verified", true, "unknown", "Inspect the file and retry; the previous backup is at "+m.Path+".bak.", err)
	}
	_ = warnings
	if written != cfg {
		return model.NewError(model.ErrConfig, "config", "config write verification did not match requested values", true, "unknown", "Inspect the file and retry; the previous backup is at "+m.Path+".bak.")
	}
	return nil
}

func (m Manager) Set(ctx context.Context, key, value string) (Config, error) {
	if err := m.ensureDir(); err != nil {
		return Config{}, err
	}
	lock, unlock, err := m.openWriteLock(ctx)
	if err != nil {
		return Config{}, err
	}
	defer lock.Close()
	defer unlock()
	cfg, _, _, err := m.Load(ctx)
	if err != nil {
		return Config{}, err
	}
	if err := SetValue(&cfg, key, value); err != nil {
		return Config{}, model.WrapError(model.ErrInvalidInput, "config", err.Error(), false, "invalid", "Use `tailge config show` to inspect valid settings.", err)
	}
	if err := m.saveLocked(ctx, cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func SetValue(cfg *Config, key, value string) error {
	if cfg == nil {
		return fmt.Errorf("config is nil")
	}
	candidate := *cfg
	value = strings.TrimSpace(value)
	switch key {
	case "refresh_interval":
		d, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("refresh_interval: %w", err)
		}
		candidate.RefreshInterval = d
	case "operation_timeout":
		d, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("operation_timeout: %w", err)
		}
		candidate.OperationTimeout = d
	case "sort":
		candidate.Sort = value
	case "color_theme":
		candidate.ColorTheme = value
	case "show_system_listeners":
		b, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("show_system_listeners must be true or false")
		}
		candidate.ShowSystemListeners = b
	case "show_inactive_configured_ports":
		b, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("show_inactive_configured_ports must be true or false")
		}
		candidate.ShowInactiveConfiguredPorts = b
	default:
		return fmt.Errorf("unknown config key %q", key)
	}
	if err := candidate.Validate(); err != nil {
		return err
	}
	*cfg = candidate
	return nil
}

func Parse(text string) (Config, []string, error) {
	cfg := Defaults()
	warnings := []string{}
	scanner := bufio.NewScanner(strings.NewReader(text))
	lineNo := 0
	seen := map[string]bool{}
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(stripComment(scanner.Text()))
		if line == "" || strings.HasPrefix(line, "---") {
			continue
		}
		colon := strings.IndexByte(line, ':')
		if colon <= 0 {
			return Config{}, warnings, fmt.Errorf("line %d: expected key: value", lineNo)
		}
		key := strings.TrimSpace(line[:colon])
		value := unquote(strings.TrimSpace(line[colon+1:]))
		if seen[key] {
			return Config{}, warnings, fmt.Errorf("line %d: duplicate key %q", lineNo, key)
		}
		seen[key] = true
		switch key {
		case "version":
			v, err := strconv.Atoi(value)
			if err != nil {
				return Config{}, warnings, fmt.Errorf("line %d: version must be an integer", lineNo)
			}
			cfg.Version = v
		case "refresh_interval":
			d, err := time.ParseDuration(value)
			if err != nil {
				return Config{}, warnings, fmt.Errorf("line %d: invalid refresh_interval: %w", lineNo, err)
			}
			cfg.RefreshInterval = d
		case "sort":
			cfg.Sort = value
		case "color_theme":
			cfg.ColorTheme = value
		case "show_system_listeners":
			b, err := strconv.ParseBool(value)
			if err != nil {
				return Config{}, warnings, fmt.Errorf("line %d: show_system_listeners must be true or false", lineNo)
			}
			cfg.ShowSystemListeners = b
		case "show_inactive_configured_ports":
			b, err := strconv.ParseBool(value)
			if err != nil {
				return Config{}, warnings, fmt.Errorf("line %d: show_inactive_configured_ports must be true or false", lineNo)
			}
			cfg.ShowInactiveConfiguredPorts = b
		case "operation_timeout":
			d, err := time.ParseDuration(value)
			if err != nil {
				return Config{}, warnings, fmt.Errorf("line %d: invalid operation_timeout: %w", lineNo, err)
			}
			cfg.OperationTimeout = d
		case "serve_probe_version":
			cfg.ServeProbeVersion = value
		case "funnel_probe_version":
			cfg.FunnelProbeVersion = value
		case "serve_probe_at":
			cfg.ServeProbeAt = value
		case "funnel_probe_at":
			cfg.FunnelProbeAt = value
		default:
			warnings = append(warnings, fmt.Sprintf("ignored unknown config field %q on line %d", key, lineNo))
		}
	}
	if err := scanner.Err(); err != nil {
		return Config{}, warnings, fmt.Errorf("config input exceeds the line limit: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, warnings, err
	}
	return cfg, warnings, nil
}

func (m Manager) validateParent() error {
	info, err := os.Lstat(m.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return model.WrapError(model.ErrConfig, "config", "cannot inspect "+m.Dir, false, "unavailable", "Check the path permissions and retry.", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return model.NewError(model.ErrConfig, "config", m.Dir+" is not a regular directory", false, "unsafe", "Replace the path with a directory and retry.")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return model.NewError(model.ErrConfig, "config", m.Dir+" permissions are too broad", false, "unsafe", "Restrict the directory to owner-only permissions (0700) and retry.")
	}
	return nil
}

func (m Manager) ensureDir() error {
	if err := m.validateParent(); err != nil {
		return err
	}
	if err := os.Mkdir(m.Dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return model.WrapError(model.ErrConfig, "config", "cannot create "+m.Dir, true, "unavailable", "Check home-directory permissions and disk space, then retry.", err)
	}
	fd, err := syscall.Open(m.Dir, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return model.WrapError(model.ErrConfig, "config", "cannot open "+m.Dir+" without following links", false, "unsafe", "Replace the config directory with a real owner-only directory and retry.", err)
	}
	dir := os.NewFile(uintptr(fd), m.Dir)
	defer dir.Close()
	info, err := dir.Stat()
	if err != nil || !info.IsDir() {
		if err == nil {
			err = fmt.Errorf("path is not a directory")
		}
		return model.WrapError(model.ErrConfig, "config", "config directory validation failed", false, "unsafe", "Replace the config directory with a real owner-only directory and retry.", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return model.NewError(model.ErrConfig, "config", m.Dir+" permissions are too broad", false, "unsafe", "Restrict the directory to owner-only permissions (0700) and retry.")
	}
	if err := dir.Chmod(0o700); err != nil {
		return model.WrapError(model.ErrConfig, "config", "cannot restrict "+m.Dir+" permissions", false, "unsafe", "Fix directory permissions and retry.", err)
	}
	return nil
}

func (m Manager) openWriteLock(ctx context.Context) (*os.File, func(), error) {
	lockPath := m.Path + ".lock"
	if info, statErr := os.Lstat(lockPath); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
			return nil, nil, model.NewError(model.ErrConfig, "config", "refusing unsafe config lock path "+lockPath, false, "unsafe", "Replace the lock path with a regular file restricted to owner permissions and retry.")
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return nil, nil, model.WrapError(model.ErrConfig, "config", "cannot inspect config lock", true, "unavailable", "Check directory permissions and retry.", statErr)
	}
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, nil, model.WrapError(model.ErrConfig, "config", "cannot open config lock", true, "unavailable", "Check directory permissions and retry.", err)
	}
	if err := lock.Chmod(0o600); err != nil {
		_ = lock.Close()
		return nil, nil, model.WrapError(model.ErrConfig, "config", "cannot restrict config lock permissions", false, "unsafe", "Fix the lock permissions and retry.", err)
	}
	if err := lockConfig(ctx, lock); err != nil {
		_ = lock.Close()
		return nil, nil, err
	}
	return lock, func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }, nil
}

func lockConfig(ctx context.Context, file *os.File) error {
	for {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return configContextError(ctxErr)
		}
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return model.WrapError(model.ErrConfig, "config", "cannot lock config for writing", true, "busy", "Retry after another tailge process finishes.", err)
		}
		select {
		case <-ctx.Done():
			return configContextError(ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func configContextError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return model.WrapError(model.ErrTimeout, "config", "config lock acquisition timed out", true, "timeout", "Retry after another tailge process finishes.", err)
	}
	return model.WrapError(model.ErrCancelled, "config", "config lock acquisition cancelled", true, "cancelled", "Retry the command.", err)
}

func copyFile(dst, src string, mode os.FileMode) error {
	in, err := openRegular(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.CreateTemp(filepath.Dir(dst), ".copy.tmp-")
	if err != nil {
		return err
	}
	tmpName := out.Name()
	defer os.Remove(tmpName)
	if err := out.Chmod(mode); err != nil {
		out.Close()
		return err
	}
	n, err := io.Copy(out, io.LimitReader(in, maxConfigBytes+1))
	if err != nil {
		out.Close()
		return err
	}
	if n > maxConfigBytes {
		out.Close()
		return fmt.Errorf("file exceeds %d-byte limit", maxConfigBytes)
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, dst)
}

func readBounded(path string) ([]byte, error) {
	file, err := openRegular(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxConfigBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxConfigBytes {
		return nil, fmt.Errorf("file exceeds %d-byte limit", maxConfigBytes)
	}
	return data, nil
}

func openRegular(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		file.Close()
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		file.Close()
		return nil, fmt.Errorf("%s has permissions broader than 0600", path)
	}
	return file, nil
}

func stripComment(line string) string {
	quoted := false
	for i, r := range line {
		switch r {
		case '"':
			quoted = !quoted
		case '#':
			if !quoted {
				return line[:i]
			}
		}
	}
	return line
}

func unquote(value string) string {
	if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'')) {
		return value[1 : len(value)-1]
	}
	return value
}
