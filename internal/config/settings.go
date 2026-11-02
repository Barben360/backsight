package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
)

// NumericSettings is every numeric field of the [recall] and [feedback]
// tables, keyed recall.<key> and feedback.<key> (their toml keys); a
// boolean reads 0 or 1 and a string is left out.
func NumericSettings(cfg Config) map[string]float64 {
	out := map[string]float64{}
	for _, t := range []struct {
		prefix string
		v      any
	}{{"recall.", cfg.Recall}, {"feedback.", cfg.Feedback}} {
		rv := reflect.ValueOf(t.v)
		for i := range rv.NumField() {
			key, _, _ := strings.Cut(rv.Type().Field(i).Tag.Get("toml"), ",")
			if key == "" || key == "-" {
				continue
			}
			f := rv.Field(i)
			switch f.Kind() {
			case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
				out[t.prefix+key] = float64(f.Int())
			case reflect.Float32, reflect.Float64:
				out[t.prefix+key] = f.Float()
			case reflect.Bool:
				if f.Bool() {
					out[t.prefix+key] = 1
				} else {
					out[t.prefix+key] = 0
				}
			}
		}
	}
	return out
}

// SettingsSHA256 is the hex SHA-256 of the canonical JSON of m (keys
// sorted), so equal values hash alike whatever the insertion order.
func SettingsSHA256(m map[string]float64) string {
	raw, _ := json.Marshal(m) // encoding/json sorts map keys; floats are finite
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
