package backup

import (
	"context"
	"time"
)

// RestoreParams 只会解析到已登记根下的一个目录；根自身、绝对路径和符号链接都拒绝。
type RestoreParams struct {
	RootID    string
	Path      string
	Overwrite bool
}

type RestoreProbe struct {
	Exists         bool
	Empty          bool
	TargetVersion  string
	CapacityKnown  bool
	AvailableBytes uint64
}

type RestoreEntry struct {
	Path   string
	Type   string
	Mode   uint32
	Size   int64
	SHA256 string
}

type RestoreReceipt struct {
	RootID       string
	Path         string
	Entries      int
	Bytes        int64
	VerifiedAt   time.Time
	CleanupError string
}

type RestoreSession interface {
	Target() string
	Inspect(context.Context) ([]RestoreEntry, error)
	Publish(context.Context, []RestoreEntry) (RestoreReceipt, error)
	Abort(context.Context) error
}

func EqualRestoreEntries(a, b []RestoreEntry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
