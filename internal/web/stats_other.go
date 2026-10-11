//go:build !linux

package web

func procRSS() (rssBreakdown, bool) { return rssBreakdown{}, false }

func mountUsage(_ string) (mountStats, uint64, bool) { return mountStats{}, 0, false }

func readVmRSS() uint64 { return 0 }

func readRSSOf(_ string) uint64 { return 0 }

func readPeakRSS(_ string) uint64 { return 0 }

func resetPeakRSS(_ string) {}

func procRSSAt(_ int) (rssBreakdown, bool) { return rssBreakdown{}, false }
