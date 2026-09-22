package tui

// Workspace banners, status feedback, readiness explanations, and theme-aware text painting.

import (
	"os"
	"strings"

	"github.com/arrokh/tailge/internal/fault"
)

func (m *workspaceModel) setBanner(value string, sticky bool) {
	m.banner, m.bannerSticky = sanitizeTUIText(value), sticky
}

func (m *workspaceModel) appendBanner(value string, sticky bool) {
	value = sanitizeTUIText(value)
	if value == "" || strings.Contains(m.banner, value) {
		m.bannerSticky = m.bannerSticky || sticky
		return
	}
	if m.banner == "" {
		m.banner = value
	} else {
		m.banner += " | " + value
	}
	m.bannerSticky = m.bannerSticky || sticky
}

func safeMessage(err error) string {
	if err == nil {
		return ""
	}
	return sanitizeTUIText(fault.AsAppError(err).Message)
}

func (m *workspaceModel) applyStatus(message statusMsg) {
	if message.sticky {
		m.setBanner(message.value, true)
	} else {
		m.transient = message.value
	}
}

func paint(theme, code, value string) string {
	if (theme != "dark" && theme != "light" && theme != "auto") || os.Getenv("NO_COLOR") != "" {
		return value
	}
	if theme == "auto" {
		theme = "dark"
	}
	if theme == "light" {
		if code == "1;36" {
			code = "1;34"
		} else if code == "33" {
			code = "1;31"
		}
	}
	return "\x1b[" + code + "m" + value + "\x1b[0m"
}
