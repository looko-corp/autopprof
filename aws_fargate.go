//go:build linux
// +build linux

package autopprof

import (
	"bufio"
	"fmt"
	"os"
	"path"
	"strconv"
	"time"

	"github.com/containerd/cgroups"
	v1 "github.com/containerd/cgroups/stats/v1"
)

const (
	limitPerVCPU = 1206340307240 // maybe
)

type awsFargate struct {
	staticPath   string
	mountPoint   string
	cpuSubsystem string
	vCPUSize     float64

	cpuQuota float64

	q cpuUsageSnapshotQueuer
}

func newAWSFargate(vcpuSize float64) *awsFargate {
	q := newCPUUsageSnapshotQueue(
		cpuUsageSnapshotQueueSize,
	)
	return &awsFargate{
		staticPath:   "/",
		mountPoint:   cgroupV1MountPoint,
		cpuSubsystem: cgroupV1CPUSubsystem,
		q:            q,
		vCPUSize:     vcpuSize,
	}
}

func (c *awsFargate) setCPUQuota() error {
	return nil
}

func (c *awsFargate) snapshotCPUUsage(usage uint64) {
	c.q.enqueue(&cpuUsageSnapshot{
		usage:     usage,
		timestamp: time.Now(),
	})
}

func (c *awsFargate) stat() (*v1.Metrics, error) {
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

func (c *awsFargate) cpuUsage() (float64, error) {
	stat, err := c.stat()
	if err != nil {
		return 0, err
	}

	c.snapshotCPUUsage(stat.CPU.Usage.Total) // In nanoseconds.

	totalUsage := float64(stat.CPU.Usage.Total)

	// Calculate the usage only if there are enough snapshots.
	if !c.q.isFull() {
		return 0, nil
	}

	cpuLimit := float64(limitPerVCPU) * c.vCPUSize

	return totalUsage / cpuLimit, nil
}

func (c *awsFargate) memUsage() (float64, error) {
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

func (c *awsFargate) parseCPU(filename string) (int, error) {
	fullpath := path.Join(c.mountPoint, c.cpuSubsystem, filename)
	fmt.Println("@@ autopprof @@ fullpath = ", fullpath)

	f, err := os.Open(
		path.Join(c.mountPoint, c.cpuSubsystem, filename),
	)
	if err != nil {
		return 0, err
	}
	scanner := bufio.NewScanner(f)
	if scanner.Scan() {
		scanned := scanner.Text()
		fmt.Println("@@ autopprof @@ scanned = ", scanned)

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
