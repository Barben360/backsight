package metrics

import (
	"context"
	"database/sql"
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/index"
)

// ServerTimeout bounds each server read of a view, per endpoint key: the
// pages of one key (the queue's two totals, the failed operations' pages)
// share it, so a view waits at most this long for the server. Tests lower
// it.
var ServerTimeout = 3 * time.Second

// doctorTimeout bounds the live doctor run of a view: past it the run is
// abandoned (its probes may ignore the context); tests lower it.
var doctorTimeout = 10 * time.Second

// Reader is the read side of the metrics index (*index.Index): a view runs
// in one read-only transaction and never writes the index.
type Reader interface {
	ReadTx(ctx context.Context, fn func(*sql.Tx) error) error
}

var _ Reader = (*index.Index)(nil)

// ServerReader is the server's read-only method set (*hindsight.ReadOnly).
type ServerReader interface {
	Version(ctx context.Context) (hindsight.Version, error)
	BankStats(ctx context.Context) (hindsight.BankStats, error)
	Memory(ctx context.Context, id string) (hindsight.MemoryDetail, error)
	ProjectTags(ctx context.Context) ([]hindsight.TagCount, error)
	MemoriesTimeseries(ctx context.Context, period string) (hindsight.Timeseries, error)
	LLMStats(ctx context.Context, period, operation string) ([]hindsight.LLMBucket, error)
	OperationsPage(ctx context.Context, status, typ string, limit, offset int) (hindsight.OperationPage, error)
}

var _ ServerReader = (*hindsight.ReadOnly)(nil)

// Source is what a view reads: the index caught up by the caller, the
// clock, the configuration, the server (nil when none is configured) and
// the live doctor run (nil: doctor.check reads no_data).
type Source struct {
	Index      Reader
	Fresh      index.Freshness
	Now        func() time.Time
	Config     config.Config
	Configured bool         // a config.toml was read
	Server     ServerReader // nil: none configured
	ServerHost string       // host:port of server.url
	Doctor     func(ctx context.Context) ([]DoctorCheck, error)
}

// now is the source's clock, the wall clock when it has none.
func (s Source) now() time.Time {
	if s.Now == nil {
		return time.Now()
	}
	return s.Now()
}
