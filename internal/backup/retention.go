package backup

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

// RetentionSelection 是一次显式清理的纯计算结果。执行端只接收完整 id，
// 不接受调用方另传一份“待删列表”。
type RetentionSelection struct {
	Keep   []Snapshot
	Delete []Snapshot
}

// SelectRetention 按 keep_last、keep_daily、keep_weekly 的并集保留快照。
// daily/weekly 使用 UTC 日历桶，每个桶保留最新一条，再取最近 N 个非空桶。
func SelectRetention(snapshots []Snapshot, retention Retention) (RetentionSelection, error) {
	if !retention.Configured() || retention.KeepLast < 0 || retention.KeepDaily < 0 || retention.KeepWeekly < 0 ||
		retention.KeepLast > MaxRetentionCount || retention.KeepDaily > MaxRetentionCount || retention.KeepWeekly > MaxRetentionCount {
		return RetentionSelection{}, errors.New("backup: 清理需要有效的 keep_last/keep_daily/keep_weekly 保留策略")
	}
	ordered := append([]Snapshot(nil), snapshots...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Time.Equal(ordered[j].Time) {
			return ordered[i].ID > ordered[j].ID
		}
		return ordered[i].Time.After(ordered[j].Time)
	})
	keep := make(map[string]struct{}, len(ordered))
	for i, snap := range ordered {
		if !resticIDPattern.MatchString(snap.ID) {
			return RetentionSelection{}, errors.New("backup: 保留计算收到无效快照 id")
		}
		if i < retention.KeepLast {
			keep[snap.ID] = struct{}{}
		}
	}
	keepPeriods(ordered, retention.KeepDaily, func(t time.Time) string { return t.UTC().Format("2006-01-02") }, keep)
	keepPeriods(ordered, retention.KeepWeekly, func(t time.Time) string {
		t = t.UTC()
		year, week := t.ISOWeek()
		return fmt.Sprintf("%04d-%02d", year, week)
	}, keep)
	out := RetentionSelection{Keep: make([]Snapshot, 0, len(keep)), Delete: make([]Snapshot, 0)}
	for _, snap := range ordered {
		if _, ok := keep[snap.ID]; ok {
			out.Keep = append(out.Keep, snap)
		} else {
			out.Delete = append(out.Delete, snap)
		}
	}
	return out, nil
}

func keepPeriods(ordered []Snapshot, limit int, bucket func(time.Time) string, keep map[string]struct{}) {
	if limit == 0 {
		return
	}
	seen := make(map[string]struct{})
	for _, snap := range ordered {
		key := bucket(snap.Time)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		keep[snap.ID] = struct{}{}
		if len(seen) == limit {
			return
		}
	}
}
