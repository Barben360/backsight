package retain

import (
	"context"
	"fmt"
	"strings"

	"github.com/Barben360/backsight/internal/hindsight"
)

// Marker is how backsight knows a bank beside the memory bank as its own
// (decision 0067, block 4 review): the name it gives the bank, and the
// prefix of every document id it writes there.
type Marker struct {
	Name     string
	IDPrefix string
}

// ControlMarker marks the control bank: its documents are EpochDocument and
// TombstonesDocument.
var ControlMarker = Marker{Name: "backsight control", IDPrefix: "backsight:"}

// ForeignBank is a bank backsight would set up that exists without its
// marker: someone else's, which backsight leaves as it is.
type ForeignBank struct {
	Bank   string
	Reason string
}

func (e *ForeignBank) Error() string {
	return "bank " + e.Bank + " is not backsight's (" + e.Reason + "), so backsight leaves it as it is"
}

// OwnedBank reads bank and says whether backsight may set it up: missing
// (exists false), or carrying m (named m.Name, every document id starting
// with m.IDPrefix), when its documents are returned. Any other bank is a
// *ForeignBank. It only reads.
func OwnedBank(ctx context.Context, cl *hindsight.Client, bank string, m Marker) (docs []hindsight.DocumentItem, exists bool, err error) {
	b, found, err := cl.FindBank(ctx, bank)
	if err != nil || !found {
		return nil, false, err
	}
	if b.Name != m.Name {
		return nil, true, &ForeignBank{bank, fmt.Sprintf("its name is %q, not %q", b.Name, m.Name)}
	}
	docs, err = cl.ListDocuments(ctx, bank)
	if err != nil {
		return nil, true, err
	}
	other := 0
	for _, d := range docs {
		if !strings.HasPrefix(d.ID, m.IDPrefix) {
			other++
		}
	}
	if other > 0 {
		return nil, true, &ForeignBank{bank, fmt.Sprintf("it holds %d document(s) whose id does not start with %q", other, m.IDPrefix)}
	}
	return docs, true, nil
}
