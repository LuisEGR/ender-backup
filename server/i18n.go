package main

import (
	"net/http"
	"os"
	"strings"
	"sync"
)

// Languages Ender Backup speaks. The settings page has its own table of the same
// languages (ui.html, STRINGS); this one covers what the server shows itself: the native
// pairing and folder dialogs. To add a language, add it here, to `messages`, and to the
// page's STRINGS.
var languages = []string{"en", "es"}

// messages: English text → translation, per language.
var messages = map[string]map[string]string{
	"es": {
		"“%s” wants to pair with this computer.\n\nOnly approve if you just tapped this computer in the Ender app.": "“%s” quiere vincularse con esta computadora.\n\nAprueba solo si acabas de tocar esta computadora en la app Ender.",
		"Yes = Approve, No = Deny":               "Sí = Aprobar, No = Rechazar",
		"Approve":                                "Aprobar",
		"Deny":                                   "Rechazar",
		"Choose where to save photos and videos": "Elige dónde guardar las fotos y los videos",
	},
}

func supportedLanguage(l string) bool {
	for _, s := range languages {
		if s == l {
			return true
		}
	}
	return false
}

// baseLanguage turns "es-MX", "es_ES.UTF-8" and the like into "es".
func baseLanguage(tag string) string {
	tag = strings.ToLower(strings.TrimSpace(tag))
	if i := strings.IndexAny(tag, "-_.@"); i >= 0 {
		tag = tag[:i]
	}
	return tag
}

// cachedOSLanguage asks the system once (on macOS that runs a command).
var cachedOSLanguage = sync.OnceValue(osLanguage)

// systemLanguage is the computer's language if Ender speaks it, else English.
func systemLanguage() string {
	for _, env := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if v := os.Getenv(env); v != "" && v != "C" && v != "POSIX" {
			if l := baseLanguage(v); supportedLanguage(l) {
				return l
			}
			break
		}
	}
	if l := baseLanguage(cachedOSLanguage()); supportedLanguage(l) {
		return l
	}
	return "en"
}

// language is the one chosen in the settings, or the computer's.
func (s *Server) language() string {
	s.mu.RLock()
	l := s.lang
	s.mu.RUnlock()
	if supportedLanguage(l) {
		return l
	}
	return systemLanguage()
}

// languageSetting is what the settings say: "" when following the computer.
func (s *Server) languageSetting() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lang
}

// tr translates an English message into lang (English if there's no translation).
func tr(lang, msg string) string {
	if t, ok := messages[lang][msg]; ok {
		return t
	}
	return msg
}

// handleLanguage sets the language: "" follows the computer's.
func (s *Server) handleLanguage(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Language string `json:"language"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Language != "" && !supportedLanguage(req.Language) {
		writeError(w, http.StatusBadRequest, "unsupported language")
		return
	}
	s.mu.Lock()
	s.lang = req.Language
	s.mu.Unlock()
	if err := s.updateConfig(func(c *Config) { c.Language = req.Language }); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "language": s.language()})
}
