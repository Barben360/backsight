package fileimport

import (
	"slices"
	"strconv"

	"github.com/Barben360/backsight/internal/state"
)

// DocumentPrefix starts every imported document's id.
const DocumentPrefix = "import:"

// SourceID names one import root on one machine: the first 12 hex
// characters of sha256(machine_id + "\n" + absolute root).
func SourceID(machineID, absRoot string) string {
	return hashOf([]byte(machineID + "\n" + absRoot))[:12]
}

// DocumentID is import:<source>/<rel>, or its first successor <id>#2,
// <id>#3… that no tombstone names: a purged id is never written again.
func DocumentID(source, rel string, tombs []state.Tombstone) string {
	base := DocumentPrefix + source + "/" + rel
	id := base
	for n := 2; slices.ContainsFunc(tombs, func(t state.Tombstone) bool { return t.Document == id }); n++ {
		id = base + "#" + strconv.Itoa(n)
	}
	return id
}

// PresetSourceID names a preset's import of root on one machine, apart
// from a plain import of the same folder: the first 12 hex characters of
// sha256(machine_id + "\n" + absolute root + "\n" + preset).
func PresetSourceID(machineID, preset, absRoot string) string {
	return hashOf([]byte(machineID + "\n" + absRoot + "\n" + preset))[:12]
}
