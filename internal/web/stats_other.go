//go:build !linux

package web

import "github.com/monbooru/monbooru/internal/shared/desktop"

func procRSS() (rssBreakdown, bool) { return rssBreakdown{}, false }

func mountUsage(path string) (mountStats, uint64, bool) {
	total, free, err := desktop.DiskUsage(path)
	if err != nil {
		return mountStats{}, 0, false
	}
	return usageStats(total, free), 0, true
}

func readVmRSS() uint64 { return 0 }

func readRSSOf(_ string) uint64 { return 0 }

func readPeakRSS(_ string) uint64 { return 0 }

func resetPeakRSS(_ string) {}

func procRSSAt(_ int) (rssBreakdown, bool) { return rssBreakdown{}, false }
