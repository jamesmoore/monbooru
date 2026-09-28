//go:build !linux

package web

func procRSS() (rssBreakdown, bool) { return rssBreakdown{}, false }

func mountUsage(_ string) (mountStats, uint64, bool) { return mountStats{}, 0, false }

func readVmRSS() uint64 { return 0 }

func readVmHWM() uint64 { return 0 }

func procRSSAt(_ int) (rssBreakdown, bool) { return rssBreakdown{}, false }
