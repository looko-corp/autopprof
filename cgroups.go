//go:build linux
// +build linux

package autopprof

import (
	"fmt"

	"github.com/containerd/cgroups"
)

//go:generate mockgen -source=cgroups.go -destination=cgroups_mock.go -package=autopprof

const (
	cpuUsageSnapshotQueueSize = 24 // 24 * 5s = 2 minutes.
)

type queryer interface {
	cpuUsage() (float64, error)
	memUsage() (float64, error)

	setCPUQuota() error
}

func newQueryer() (queryer, error) {
	switch cgroups.Mode() {
	case cgroups.Legacy:
		fmt.Println("@@ autopprof @@: Cgroup Version = newCgroupsV1")
		return newCgroupsV1(), nil
	case cgroups.Hybrid, cgroups.Unified:
		fmt.Println("@@ autopprof @@: Cgroup Version = newCgroupsV2")
		return newCgroupsV2(), nil
	}
	return nil, ErrCgroupsUnavailable
}
