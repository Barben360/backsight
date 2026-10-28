package retain

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	"github.com/Barben360/backsight/internal/hindsight"
)

// ServerDocument holds the server's own id, in the bank's control bank: it
// is stored with the server's data, so a restore keeps it, and clients can
// tell a different server from theirs by it.
const ServerDocument = "backsight:server"

// ReadServerID reads the server id of bank (the memory bank) from its
// control bank. No control bank or no document is "" (a server before its
// id was written); a document without a server_id, or any other failure,
// is an error: the caller must not act on a guess.
func ReadServerID(ctx context.Context, cl *hindsight.Client, bank string) (string, error) {
	doc, err := cl.GetDocument(ctx, ControlBank(bank), ServerDocument)
	var apiErr *hindsight.APIError
	switch {
	case errors.As(err, &apiErr) && apiErr.Kind == hindsight.NotFound:
		return "", nil
	case err != nil:
		return "", errors.New(errSummary(err))
	}
	id := doc.Metadata["server_id"]
	if id == "" {
		return "", fmt.Errorf("%s: no server_id in its metadata", ServerDocument)
	}
	return id, nil
}

// EnsureServerID returns bank's server id, writing a new random one
// (UUIDv4, synchronously, no LLM call) when the server has none yet.
func EnsureServerID(ctx context.Context, cl *hindsight.Client, bank string) (string, error) {
	id, err := ReadServerID(ctx, cl, bank)
	if err != nil || id != "" {
		return id, err
	}
	if id, err = newUUID(); err != nil {
		return "", err
	}
	if err := quietControl(ctx, cl, bank, false); err != nil {
		return "", err
	}
	at := time.Now().UTC().Format(time.RFC3339)
	return id, cl.RetainNow(ctx, ControlBank(bank), []hindsight.RetainItem{{
		Content:    "server " + id + " (created at " + at + ")",
		DocumentID: ServerDocument,
		Tags:       []string{},
		Metadata:   map[string]string{"server_id": id, "created_at": at},
	}})
}

// newUUID is a random (version 4) UUID.
func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}
