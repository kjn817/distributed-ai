// Package hardware detects CPU, RAM, storage and GPU.
//
// Detection logic is preserved verbatim from the original runtime. The only
// structural change: command-output parsing is split into pure functions
// (parseMacGPUOutput, parseWindowsGPUJSON) so behavior is unit-testable
// without touching the exec paths.
package hardware

import (
	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/mem"
)

// DetectHardware returns CPU model, physical cores, logical cores, RAM GB.
func DetectHardware() (cpuModel string, physicalCores, logicalCores int, ramGB float64) {
	cpuModel = "Unknown"

	if info, err := cpu.Info(); err == nil && len(info) > 0 {
		if info[0].ModelName != "" {
			cpuModel = info[0].ModelName
		}
	}

	physicalCores, _ = cpu.Counts(false)
	logicalCores, _ = cpu.Counts(true)

	if vm, err := mem.VirtualMemory(); err == nil {
		ramGB = float64(vm.Total) / 1024 / 1024 / 1024
	}

	return cpuModel, physicalCores, logicalCores, ramGB
}

// DetectStorage returns total, used, free GB of the root filesystem.
func DetectStorage() (totalGB, usedGB, freeGB float64) {
	usage, err := disk.Usage("/")
	if err != nil {
		return 0, 0, 0
	}

	totalGB = float64(usage.Total) / 1024 / 1024 / 1024
	usedGB = float64(usage.Used) / 1024 / 1024 / 1024
	freeGB = float64(usage.Free) / 1024 / 1024 / 1024

	return totalGB, usedGB, freeGB
}
