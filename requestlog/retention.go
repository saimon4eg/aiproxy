package requestlog

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

// RetentionReport says what one retention pass did.
type RetentionReport struct {
	Compressed int
	Removed    int
	IndexRows  int64
	// Evicted is how many day directories or summary files were deleted because
	// the log tree outgrew its size cap, and Freed how many bytes that reclaimed.
	Evicted int
	Freed   int64
}

// ApplyRetention brings the log tree back inside its two limits: age and size.
//
// Age first — day directories older than RetentionDays are compressed and then
// dropped once they are twice as old; the daily summary files follow the same
// lifetime. Size second — if the tree is still larger than MaxBytes, the oldest
// units are deleted until it fits. Age alone is not a bound: a day that produces
// ten times the usual volume still fits the retention window and still fills the
// disk, so both are enforced here.
//
// The measured rate was 250 MB/day with no TTL at all, so this is the only thing
// standing between the volume and a full disk. Day directories are what gets
// compressed, not the daily summary files: bodies are the bulk and are read
// rarely, one directory at a time.
func (l *Logger) ApplyRetention(now time.Time) (RetentionReport, error) {
	var report RetentionReport
	if l == nil {
		return report, nil
	}
	dir := l.cfg.Dir
	dropAfter := now
	if l.cfg.RetentionDays > 0 {
		cutoff := now.AddDate(0, 0, -l.cfg.RetentionDays)
		dropAfter = now.AddDate(0, 0, -2*l.cfg.RetentionDays)

		for _, day := range SortedDays(dir) {
			date, err := time.ParseInLocation("2006-01-02", day, time.Local)
			if err != nil {
				continue
			}
			path := filepath.Join(dir, day)
			if date.After(cutoff) {
				continue
			}
			compressed, removed, err := compressDay(path)
			report.Compressed += compressed
			report.Removed += removed
			if err != nil {
				slog.Warn("requestlog: retention failed for a day", "day", day, "error", err)
				continue
			}
			if !date.After(dropAfter) {
				if err := os.RemoveAll(path); err == nil {
					report.Removed++
				}
			}
		}

		// Daily summary files follow the same lifetime, compressed in place.
		entries, err := os.ReadDir(dir)
		if err == nil {
			for _, entry := range entries {
				if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
					continue
				}
				day := dayFromSummaryName(entry.Name())
				date, err := time.ParseInLocation("2006-01-02", day, time.Local)
				if err != nil || date.After(cutoff) {
					continue
				}
				if err := compressFile(filepath.Join(dir, entry.Name())); err != nil {
					slog.Warn("requestlog: compressing summary failed", "file", entry.Name(), "error", err)
					continue
				}
				report.Compressed++
			}
		}
	}

	evicted, freed := l.evictToSizeCap(now)
	report.Evicted += evicted
	report.Freed += freed

	if l.idx != nil && l.cfg.RetentionDays > 0 {
		rows, err := l.idx.prune(dropAfter)
		if err != nil {
			slog.Warn("requestlog: pruning index failed", "error", err)
		} else {
			report.IndexRows = rows
		}
	}
	return report, nil
}

// logUnit is one deletable piece of the log tree: a day directory or a daily
// summary file, with the date it describes.
type logUnit struct {
	path string
	size int64
	date time.Time
}

// evictToSizeCap deletes the oldest units until the tree fits in MaxBytes.
//
// What goes first is whatever is oldest — the least likely to be read — and the day
// currently being written is never touched: its files are open and its summaries are
// what the current window is accounted from.
func (l *Logger) evictToSizeCap(now time.Time) (int, int64) {
	if l.cfg.MaxBytes <= 0 {
		return 0, 0
	}
	units, total := l.logUnits()
	if total <= l.cfg.MaxBytes {
		return 0, 0
	}
	// Oldest first; within a day the directory sorts before its summary files, and
	// the directory is the bulk.
	sort.Slice(units, func(i, j int) bool {
		if !units[i].date.Equal(units[j].date) {
			return units[i].date.Before(units[j].date)
		}
		return units[i].path < units[j].path
	})

	today := now.Format("2006-01-02")
	var evicted int
	var freed int64
	for _, unit := range units {
		if total <= l.cfg.MaxBytes {
			break
		}
		if unit.date.Format("2006-01-02") == today {
			continue
		}
		if err := os.RemoveAll(unit.path); err != nil {
			slog.Warn("requestlog: evicting failed", "path", unit.path, "error", err)
			continue
		}
		evicted++
		freed += unit.size
		total -= unit.size
	}
	if evicted > 0 {
		slog.Info("requestlog: size cap evicted the oldest logs",
			"evicted", evicted, "freed_bytes", freed, "cap_bytes", l.cfg.MaxBytes)
	}
	return evicted, freed
}

// logUnits lists the deletable units of the log tree and their total size. The index
// and anything unrecognised are left out: they are small, and the index is the only
// way to query summaries.
func (l *Logger) logUnits() ([]logUnit, int64) {
	entries, err := os.ReadDir(l.cfg.Dir)
	if err != nil {
		return nil, 0
	}
	var units []logUnit
	var total int64
	for _, entry := range entries {
		path := filepath.Join(l.cfg.Dir, entry.Name())
		name := entry.Name()
		if entry.IsDir() {
			date, err := time.ParseInLocation("2006-01-02", name, time.Local)
			if err != nil {
				continue // not a day directory
			}
			size := treeSize(path)
			units = append(units, logUnit{path: path, size: size, date: date})
			total += size
			continue
		}
		day := dayFromSummaryName(name)
		if day == "" {
			continue // index.sqlite and friends
		}
		date, err := time.ParseInLocation("2006-01-02", day, time.Local)
		if err != nil {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		units = append(units, logUnit{path: path, size: info.Size(), date: date})
		total += info.Size()
	}
	return units, total
}

// treeSize is the total size of every regular file under dir.
func treeSize(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		if info, err := entry.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

// dayFromSummaryName extracts YYYY-MM-DD from "requests-2026-11-12.jsonl" and from
// its compressed form "requests-2026-11-12.jsonl.zst".
func dayFromSummaryName(name string) string {
	trimmed := strings.TrimSuffix(name, ".zst")
	trimmed = strings.TrimSuffix(trimmed, ".jsonl")
	if !strings.HasPrefix(trimmed, "requests-") && !strings.HasPrefix(trimmed, "errors-") {
		return ""
	}
	// The day itself contains dashes, so take the last ten characters.
	if len(trimmed) < 10 {
		return ""
	}
	return trimmed[len(trimmed)-10:]
}

// compressDay compresses every plain file in a day directory, then reports how
// many files were compressed.
func compressDay(dir string) (compressed, removed int, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, 0, err
	}
	for _, entry := range entries {
		if entry.IsDir() || strings.HasSuffix(entry.Name(), ".zst") {
			continue
		}
		if err := compressFile(filepath.Join(dir, entry.Name())); err != nil {
			return compressed, removed, err
		}
		compressed++
	}
	return compressed, removed, nil
}

// compressFile writes <path>.zst and removes the original, atomically enough that
// a crash leaves either the original or a complete archive.
func compressFile(path string) error {
	source, err := os.Open(path)
	if err != nil {
		return err
	}
	defer source.Close()

	tempPath := path + ".zst.tmp"
	out, err := os.Create(tempPath)
	if err != nil {
		return err
	}
	encoder, err := zstd.NewWriter(out, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		out.Close()
		os.Remove(tempPath)
		return err
	}
	if _, err := io.Copy(encoder, source); err != nil {
		encoder.Close()
		out.Close()
		os.Remove(tempPath)
		return err
	}
	if err := encoder.Close(); err != nil {
		out.Close()
		os.Remove(tempPath)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tempPath)
		return err
	}
	if err := os.Rename(tempPath, path+".zst"); err != nil {
		return err
	}
	return os.Remove(path)
}

// StartRetention runs retention in the background: once a day, and once at startup.
//
// The schedule lives in the process rather than in cron inside the image. Cleanup is
// part of what the logger owns, it needs no root, no second binary and no crontab,
// and a container that restarts keeps its schedule instead of losing it. Age and size
// are both enforced on every pass, so a busy day cannot outrun a daily run.
func (l *Logger) StartRetention(interval time.Duration) func() {
	if l == nil || (l.cfg.RetentionDays <= 0 && l.cfg.MaxBytes <= 0) {
		return func() {}
	}
	if interval <= 0 {
		interval = l.cfg.RetentionInterval
	}
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	stop := make(chan struct{})
	go func() {
		// One pass at startup: a container that is restarted often would
		// otherwise never reach the first tick and never rotate anything.
		logRetentionPass(l.ApplyRetention(time.Now()))
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case now := <-ticker.C:
				logRetentionPass(l.ApplyRetention(now))
			}
		}
	}()
	return func() { close(stop) }
}

// logRetentionPass reports a pass, and only a pass that did something: an idle day
// must not produce a line every cycle.
func logRetentionPass(report RetentionReport, err error) {
	if err != nil {
		slog.Warn("requestlog: retention pass failed", "error", err)
		return
	}
	if report.Compressed == 0 && report.Removed == 0 && report.IndexRows == 0 &&
		report.Evicted == 0 {
		return
	}
	slog.Info("requestlog: retention pass",
		"compressed", report.Compressed, "removed", report.Removed,
		"evicted", report.Evicted, "freed_bytes", report.Freed,
		"index_rows_pruned", report.IndexRows)
}
