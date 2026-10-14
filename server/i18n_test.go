package main

import "testing"

func TestBaseLanguage(t *testing.T) {
	for in, want := range map[string]string{"es-MX": "es", "es_ES.UTF-8": "es", "en": "en", "EN-us": "en", "": ""} {
		if got := baseLanguage(in); got != want {
			t.Errorf("baseLanguage(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLanguageSetting(t *testing.T) {
	t.Setenv("LC_ALL", "es_MX.UTF-8")
	s := &Server{}
	if got := s.language(); got != "es" {
		t.Fatalf("following the computer: %q, want es", got)
	}
	s.lang = "en"
	if got := s.language(); got != "en" {
		t.Fatalf("chosen English: %q", got)
	}
	if got := tr("es", "Approve"); got != "Aprobar" {
		t.Fatalf("tr: %q", got)
	}
	if got := tr("en", "Approve"); got != "Approve" {
		t.Fatalf("tr en: %q", got)
	}
}

// Every entry in the translation tables is actually translated.
func TestMessagesTranslated(t *testing.T) {
	for lang, table := range messages {
		for en, tr := range table {
			if tr == "" || tr == en {
				t.Errorf("%s: %q isn't translated", lang, en)
			}
		}
	}
}
