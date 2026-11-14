package main

import "testing"

func TestResolveVersion(t *testing.T) {
	if got := resolveVersion("1.2.3"); got != "1.2.3" {
		t.Errorf("версия из ldflags должна сохраняться: %q", got)
	}
	// В тестах модуль собран из исходников: версия "(devel)" → остаётся "dev".
	if got := resolveVersion("dev"); got != "dev" {
		t.Errorf("локальная сборка: %q", got)
	}
}
