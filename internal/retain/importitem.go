package retain

import (
	"time"

	"github.com/Barben360/backsight/internal/hindsight"
)

// ImportFile is one file of a folder import, ready to send.
type ImportFile struct {
	Source, Rel, DocumentID string
	Content                 string // redacted
	SHA256                  string // of the raw file
	Project                 string
	MachineID, Version      string
	Date                    time.Time
}

// ImportItem is the retain item of an imported file and its operation id:
// a repeated send of the same content to the same document collapses.
func ImportItem(bank string, f ImportFile) (hindsight.RetainItem, string) {
	ts := f.Date
	item := hindsight.RetainItem{
		Content: f.Content, Context: "file imported from " + f.Rel, DocumentID: f.DocumentID, Strategy: "import",
		Tags:              []string{"project:" + f.Project, "source:import"},
		ObservationScopes: [][]string{{"project:" + f.Project}},
		Timestamp:         &ts,
		Metadata: map[string]string{"source": f.Source, "path": f.Rel, "sha256": f.SHA256,
			"machine_id": f.MachineID, "binary_version": f.Version},
	}
	return item, uuid5(nsBacksight, bank+"\n"+f.DocumentID+"\n"+f.SHA256)
}
