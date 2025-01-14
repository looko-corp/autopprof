//go:build linux
// +build linux

package autopprof

import (
	"bufio"
	"os"
	"path"
	"strconv"
	"time"

	"github.com/containerd/cgroups"
	v1 "github.com/containerd/cgroups/stats/v1"
)

const (
	cgroupV1MountPoint    = "/sys/fs/cgroup"
	cgroupV1CPUSubsystem  = "cpu"
	cgroupV1CPUQuotaFile  = "cpu.cfs_quota_us"
	cgroupV1CPUPeriodFile = "cpu.cfs_period_us"

	cgroupV1UsageUnit = time.Nanosecond
)

type cgroupV1 struct {
	staticPath   string
	mountPoint   string
	cpuSubsystem string

	cpuQuota float64

	q cpuUsageSnapshotQueuer
}

func newCgroupsV1() *cgroupV1 {
	q := newCPUUsageSnapshotQueue(
		cpuUsageSnapshotQueueSize,
	)
	return &cgroupV1{
		staticPath:   "/",
		mountPoint:   cgroupV1MountPoint,
		cpuSubsystem: cgroupV1CPUSubsystem,
		q:            q,
	}
}

func (c *cgroupV1) setCPUQuota() error {
	quota, err := c.parseCPU(cgroupV1CPUQuotaFile)
	if err != nil {
		return err
	}
	period, err := c.parseCPU(cgroupV1CPUPeriodFile)
	if err != nil {
		return err
	}
	// fmt.Println("@@ autopprof @@ quota = ", quota, ", period = ", period)
	c.cpuQuota = float64(quota) / float64(period)
	return nil
}

func (c *cgroupV1) snapshotCPUUsage(usage uint64) {
	c.q.enqueue(&cpuUsageSnapshot{
		usage:     usage,
		timestamp: time.Now(),
	})
}

func (c *cgroupV1) stat() (*v1.Metrics, error) {
	var (
		path    = cgroups.StaticPath(c.staticPath)
		cg, err = cgroups.Load(cgroups.V1, path)
	)
	if err != nil {
		return nil, err
	}
	stat, err := cg.Stat()
	if err != nil {
		return nil, err
	}
	return stat, nil
}

func (c *cgroupV1) cpuUsage() (float64, error) {
	stat, err := c.stat()
	if err != nil {
		return 0, err
	}

	c.snapshotCPUUsage(stat.CPU.Usage.Total) // In nanoseconds.

	// Calculate the usage only if there are enough snapshots.
	if !c.q.isFull() {
		// fmt.Println("@@ autopprof @@ cpu is full")
		return 0, nil
	}

	s1, s2 := c.q.head(), c.q.tail()
	// fmt.Printf("@@ autopprof @@ s1 = %+v, s2 = %+v \n", s1, s2)
	delta := time.Duration(s2.usage-s1.usage) * cgroupV1UsageUnit
	duration := s2.timestamp.Sub(s1.timestamp)
	// fmt.Printf("@@ autopprof @@ delta = %+v(%+v), duration = %+v(%+v), cpuQuota = %+v \n", delta, float64(delta), duration, float64(duration), c.cpuQuota)
	return (float64(delta) / float64(duration)) / c.cpuQuota, nil
}

func (c *cgroupV1) memUsage() (float64, error) {
	stat, err := c.stat()
	if err != nil {
		return 0, err
	}
	var (
		sm    = stat.Memory
		usage = sm.Usage.Usage - sm.InactiveFile
		limit = sm.HierarchicalMemoryLimit
	)
	return float64(usage) / float64(limit), nil
}

func (c *cgroupV1) parseCPU(filename string) (int, error) {
	fullpath := path.Join(c.mountPoint, c.cpuSubsystem, filename)
	//("@@ autopprof @@ fullpath = ", fullpath)

	f, err := os.Open(fullpath)
	if err != nil {
		return 0, err
	}
	scanner := bufio.NewScanner(f)
	if scanner.Scan() {
		scanned := scanner.Text()
		//fmt.Println("@@ autopprof @@ scanned = ", scanned)

		val, err := strconv.Atoi(scanned)
		if err != nil {
			return 0, err
		}
		return val, nil
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	return 0, ErrV1CPUSubsystemEmpty
}
