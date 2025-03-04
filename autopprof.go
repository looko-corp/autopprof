//go:build linux
// +build linux

package autopprof

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"time"

	"github.com/looko-corp/autopprof/report"
)

const (
	reportTimeout = 5 * time.Second
)

type autoPprof struct {
	// watchInterval is the interval to watch the resource usages.
	// Default: 5s.
	watchInterval time.Duration

	// cpuThreshold is the cpu usage threshold to trigger profile.
	// If the cpu usage is over the threshold, the autopprof will
	//  report the cpu profile.
	// Default: 0.75. (mean 75%)
	cpuThreshold float64

	// memThreshold is the memory usage threshold to trigger profile.
	// If the memory usage is over the threshold, the autopprof will
	//  report the heap profile.
	// Default: 0.75. (mean 75%)
	memThreshold float64

	// minConsecutiveOverThreshold is the minimum consecutive
	// number of over a threshold for reporting profile again.
	// Default: 12.
	minConsecutiveOverThreshold int

	// queryer is used to query the quota and the cgroup stat.
	queryer queryer

	// profiler is used to profile the cpu and the heap memory.
	profiler profiler

	// reporter is the reporter to send the profiling reports.
	reporter report.Reporter

	// reportBoth sets whether to trigger reports for both CPU and memory when either threshold is exceeded.
	// If some profiling is disabled, exclude it.
	reportBoth bool

	// Flags to disable the profiling.
	disableCPUProf bool
	disableMemProf bool

	// stopC is the signal channel to stop the watch processes.
	stopC chan struct{}
}

// globalAp is the global autopprof instance.
var globalAp *autoPprof

// Start configures and runs the autopprof process.
func Start(opt Option) error {
	qryer, err := newQueryer()
	if err != nil {
		return err
	}
	if err := opt.validate(); err != nil {
		return err
	}

	if opt.UseAWSFargate {
		qryer = newAWSFargate(opt.VCPUSize)
	}

	profr := newDefaultProfiler(defaultCPUProfilingDuration)
	ap := &autoPprof{
		watchInterval:               defaultWatchInterval,
		cpuThreshold:                defaultCPUThreshold,
		memThreshold:                defaultMemThreshold,
		minConsecutiveOverThreshold: defaultMinConsecutiveOverThreshold,
		queryer:                     qryer,
		profiler:                    profr,
		reporter:                    opt.Reporter,
		reportBoth:                  opt.ReportBoth,
		disableCPUProf:              opt.DisableCPUProf,
		disableMemProf:              opt.DisableMemProf,
		stopC:                       make(chan struct{}),
	}
	if opt.CPUThreshold != 0 {
		ap.cpuThreshold = opt.CPUThreshold
	}
	if opt.MemThreshold != 0 {
		ap.memThreshold = opt.MemThreshold
	}
	if !ap.disableCPUProf {
		if err := ap.loadCPUQuota(); err != nil {
			return err
		}
	}

	go ap.watch()
	globalAp = ap
	return nil
}

// Stop stops the global autopprof process.
func Stop() {
	if globalAp != nil {
		globalAp.stop()
	}
}

func (ap *autoPprof) loadCPUQuota() error {
	err := ap.queryer.setCPUQuota()
	if err == nil {
		return nil
	}

	// If memory profiling is disabled and CPU quota isn't set,
	//  returns an error immediately.
	if ap.disableMemProf {
		return err
	}
	// If memory profiling is enabled, just logs the error and
	//  disables the cpu profiling.
	log.Println(
		"autopprof: disable the cpu profiling due to the CPU quota isn't set",
	)
	ap.disableCPUProf = true
	return nil
}

func (ap *autoPprof) watch() {
	go ap.watchCPUUsage()
	go ap.watchMemUsage()
	<-ap.stopC
}

func (ap *autoPprof) watchCPUUsage() {
	if ap.disableCPUProf {
		return
	}

	ticker := time.NewTicker(ap.watchInterval)
	defer ticker.Stop()

	var consecutiveOverThresholdCnt int
	for {
		select {
		case <-ticker.C:
			usage, err := ap.queryer.cpuUsage()
			fmt.Println("@@ autopprof @@ cpu usage: ", usage)

			if err != nil {
				log.Println(err)
				return
			}
			if usage < ap.cpuThreshold {
				// Reset the count if the cpu usage goes under the threshold.
				consecutiveOverThresholdCnt = 0
				continue
			}

			// If cpu utilization remains high for a short period of time, no
			//  duplicate reports are sent.
			// This is to prevent the autopprof from sending too many reports.
			if consecutiveOverThresholdCnt == 0 {
				if err := ap.reportCPUProfile(usage); err != nil {
					log.Println(fmt.Errorf(
						"autopprof: failed to report the cpu profile: %w", err,
					))
				}
				if ap.reportBoth && !ap.disableMemProf {
					memUsage, err := ap.queryer.memUsage()
					if err != nil {
						log.Println(err)
						return
					}
					if err := ap.reportHeapProfile(memUsage); err != nil {
						log.Println(fmt.Errorf(
							"autopprof: failed to report the heap profile: %w", err,
						))
					}
				}
			}

			consecutiveOverThresholdCnt++
			if consecutiveOverThresholdCnt >= ap.minConsecutiveOverThreshold {
				// Reset the count and ready to report the cpu profile again.
				consecutiveOverThresholdCnt = 0
			}
		case <-ap.stopC:
			return
		}
	}
}

func (ap *autoPprof) reportCPUProfile(cpuUsage float64) error {
	b, err := ap.profiler.profileCPU()
	if err != nil {
		return fmt.Errorf("autopprof: failed to profile the cpu: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), reportTimeout)
	defer cancel()

	ci := report.CPUInfo{
		ThresholdPercentage: ap.cpuThreshold * 100,
		UsagePercentage:     cpuUsage * 100,
	}
	bReader := bytes.NewReader(b)
	if err := ap.reporter.ReportCPUProfile(ctx, bReader, ci); err != nil {
		return err
	}
	return nil
}

func (ap *autoPprof) watchMemUsage() {
	if ap.disableMemProf {
		return
	}

	ticker := time.NewTicker(ap.watchInterval)
	defer ticker.Stop()

	var consecutiveOverThresholdCnt int
	for {
		select {
		case <-ticker.C:
			usage, err := ap.queryer.memUsage()
			if err != nil {
				log.Println(err)
				return
			}

			fmt.Println("@@ autopprof @@ mem usage: ", usage)

			if usage < ap.memThreshold {
				// Reset the count if the memory usage goes under the threshold.
				consecutiveOverThresholdCnt = 0
				continue
			}

			// If memory utilization remains high for a short period of time,
			//  no duplicate reports are sent.
			// This is to prevent the autopprof from sending too many reports.
			if consecutiveOverThresholdCnt == 0 {
				if err := ap.reportHeapProfile(usage); err != nil {
					log.Println(fmt.Errorf(
						"autopprof: failed to report the heap profile: %w", err,
					))
				}
				if ap.reportBoth && !ap.disableCPUProf {
					cpuUsage, err := ap.queryer.cpuUsage()
					if err != nil {
						log.Println(err)
						return
					}
					if err := ap.reportCPUProfile(cpuUsage); err != nil {
						log.Println(fmt.Errorf(
							"autopprof: failed to report the cpu profile: %w", err,
						))
					}
				}
			}

			consecutiveOverThresholdCnt++
			if consecutiveOverThresholdCnt >= ap.minConsecutiveOverThreshold {
				// Reset the count and ready to report the heap profile again.
				consecutiveOverThresholdCnt = 0
			}
		case <-ap.stopC:
			return
		}
	}
}

func (ap *autoPprof) reportHeapProfile(memUsage float64) error {
	b, err := ap.profiler.profileHeap()
	if err != nil {
		return fmt.Errorf("autopprof: failed to profile the heap: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), reportTimeout)
	defer cancel()

	mi := report.MemInfo{
		ThresholdPercentage: ap.memThreshold * 100,
		UsagePercentage:     memUsage * 100,
	}
	bReader := bytes.NewReader(b)
	if err := ap.reporter.ReportHeapProfile(ctx, bReader, mi); err != nil {
		return err
	}
	return nil
}

func (ap *autoPprof) stop() {
	close(ap.stopC)
}
