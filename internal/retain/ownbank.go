package retain

import (
	"context"
	"errors"
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

// AdoptedControlMarker marks a control bank backsight created for a bank
// it could not prove its own (restore with other_banks=revert, a server
// upgrade's rollback): backsight writes its epoch and tombstones there,
// but it never proves the bank backsight's, so every restore asks again
// (issue 285 review MED-2).
var AdoptedControlMarker = Marker{Name: "backsight control (adopted)", IDPrefix: "backsight:"}

// ForeignBank is a bank backsight would set up that exists without its
// marker: someone else's, which backsight leaves as it is.
type ForeignBank struct {
	Bank   string
	Reason string
	Name   string // the bank's name, as listed
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
		return nil, true, &ForeignBank{bank, fmt.Sprintf("its name is %q, not %q", b.Name, m.Name), b.Name}
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
		return nil, true, &ForeignBank{bank, fmt.Sprintf("it holds %d document(s) whose id does not start with %q", other, m.IDPrefix), b.Name}
	}
	return docs, true, nil
}

// OwnedControl reads bank's control bank: backsight's (adopted reports
// AdoptedControlMarker's), missing (exists false), or someone else's
// (*ForeignBank).
func OwnedControl(ctx context.Context, cl *hindsight.Client, bank string) (adopted, exists bool, err error) {
	_, exists, err = OwnedBank(ctx, cl, ControlBank(bank), ControlMarker)
	if f := (*ForeignBank)(nil); errors.As(err, &f) && f.Name == AdoptedControlMarker.Name {
		_, exists, err = OwnedBank(ctx, cl, ControlBank(bank), AdoptedControlMarker)
		return err == nil, exists, err
	}
	return false, exists, err
}

// quietControl sets up bank's control bank (QuietBank): an existing one
// keeps its marker, proven or adopted; a new one is adopted when adopt.
func quietControl(ctx context.Context, cl *hindsight.Client, bank string, adopt bool) error {
	cb := ControlBank(bank)
	first, second := ControlMarker, AdoptedControlMarker
	if adopt {
		first, second = second, first
	}
	_, err := QuietBank(ctx, cl, cb, first, controlMission(bank))
	if f := (*ForeignBank)(nil); errors.As(err, &f) && f.Name == second.Name {
		_, err = QuietBank(ctx, cl, cb, second, controlMission(bank))
	}
	return err
}

// WriteControlDocument writes document id into bank's control bank,
// synchronously, with content and only the metadata meta: no extraction,
// the control bank being set up first with its ownership check
// (QuietBank). A bank that is not backsight's is left alone
// (*ForeignBank).
func WriteControlDocument(ctx context.Context, cl *hindsight.Client, bank, id, content string, meta map[string]string) error {
	return writeControlDocument(ctx, cl, bank, id, content, meta, false)
}

// writeControlDocument is WriteControlDocument; adopt: a control bank it
// creates is adopted (AdoptedControlMarker).
func writeControlDocument(ctx context.Context, cl *hindsight.Client, bank, id, content string, meta map[string]string, adopt bool) error {
	if err := quietControl(ctx, cl, bank, adopt); err != nil {
		return err
	}
	return cl.RetainNow(ctx, ControlBank(bank), []hindsight.RetainItem{{Content: content, DocumentID: id, Tags: []string{}, Metadata: meta}})
}

// DeleteControlDocument deletes document id from bank's control bank:
// nothing to do when the bank or the document is missing; a bank that is
// not backsight's is left alone (*ForeignBank).
func DeleteControlDocument(ctx context.Context, cl *hindsight.Client, bank, id string) error {
	if _, exists, err := OwnedControl(ctx, cl, bank); err != nil || !exists {
		return err
	}
	err := cl.DeleteDocument(ctx, ControlBank(bank), id)
	if apiErr := (*hindsight.APIError)(nil); errors.As(err, &apiErr) && apiErr.Kind == hindsight.NotFound {
		return nil
	}
	return err
}

// ControlDocuments lists the documents of bank's control bank whose id
// starts with prefix, with their metadata: none when the control bank
// does not exist.
func ControlDocuments(ctx context.Context, cl *hindsight.Client, bank, prefix string) ([]hindsight.DocumentItem, error) {
	docs, err := cl.ListDocuments(ctx, ControlBank(bank))
	var apiErr *hindsight.APIError
	switch {
	case errors.As(err, &apiErr) && apiErr.Kind == hindsight.NotFound:
		return nil, nil
	case err != nil:
		return nil, err
	}
	var out []hindsight.DocumentItem
	for _, d := range docs {
		if strings.HasPrefix(d.ID, prefix) {
			out = append(out, d)
		}
	}
	return out, nil
}
