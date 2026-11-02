package config

import (
	"reflect"
	"strings"
	"testing"
)

// The default config's numeric settings are the map the releases table
// shows: ints, floats and booleans 0/1 under recall./feedback. + toml key,
// no string setting.
func TestNumericSettingsMatchReleases(t *testing.T) {
	got := NumericSettings(Default(dirs, "/h"))
	want := map[string]float64{"recall.min_score": 0.5, "recall.global_min_score": 0.6, "recall.prompt_hits": 3,
		"recall.session_budget_chars": 6000, "recall.families": 0, "feedback.enabled": 1, "feedback.boost_weight": 0.1}
	for k, v := range want {
		if g, ok := got[k]; !ok || g != v {
			t.Errorf("%s = %v (present %v), want %v", k, g, ok, v)
		}
	}
	for _, k := range []string{"recall.briefing", "feedback.stop_check"} {
		if _, ok := got[k]; ok {
			t.Errorf("string setting %s kept", k)
		}
	}
	for k := range got {
		if !strings.HasPrefix(k, "recall.") && !strings.HasPrefix(k, "feedback.") {
			t.Errorf("key %s outside [recall] and [feedback]", k)
		}
	}
	// A changed field changes the map.
	cfg := Default(dirs, "/h")
	cfg.Recall.Families = true
	cfg.Recall.PromptHits = 7
	if m := NumericSettings(cfg); m["recall.families"] != 1 || m["recall.prompt_hits"] != 7 {
		t.Errorf("changed settings not read: %v", m)
	}
}

func TestSettingsSHAIsOfValues(t *testing.T) {
	a := map[string]float64{}
	b := map[string]float64{}
	for _, k := range []string{"x", "y", "z", "recall.a"} {
		a[k] = float64(len(k))
	}
	for _, k := range []string{"recall.a", "z", "y", "x"} {
		b[k] = float64(len(k))
	}
	if !reflect.DeepEqual(a, b) || SettingsSHA256(a) != SettingsSHA256(b) {
		t.Error("equal maps hash differently")
	}
	if len(SettingsSHA256(a)) != 64 {
		t.Errorf("not a hex sha256: %q", SettingsSHA256(a))
	}
	b["y"] = 9
	if SettingsSHA256(a) == SettingsSHA256(b) {
		t.Error("a value change kept the hash")
	}
}
