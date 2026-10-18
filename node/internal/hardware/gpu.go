package hardware

import (
	"encoding/json"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

type windowsGPU struct {
	Name                 string `json:"Name"`
	AdapterCompatibility string `json:"AdapterCompatibility"`
	DriverVersion        string `json:"DriverVersion"`
}

// DetectGPU returns model, vendor, cores, driver, metalSupport.
// Behavior identical to the original runtime.
func DetectGPU() (model, vendor string, cores int, driver, metalSupport string) {
	switch runtime.GOOS {
	case "darwin":
		return detectMacGPU()
	case "windows":
		return detectWindowsGPU()
	default:
		return "Unknown", "", 0, "", ""
	}
}

func detectMacGPU() (string, string, int, string, string) {
	out, err := exec.Command("system_profiler", "SPDisplaysDataType").Output()
	if err != nil {
		return "Unknown", "", 0, "", ""
	}
	model, vendor, cores, metal := parseMacGPUOutput(string(out))
	return model, vendor, cores, "", metal
}

// parseMacGPUOutput is the pure parsing half of detectMacGPU.
func parseMacGPUOutput(out string) (model, vendor string, cores int, metal string) {
	model = "Unknown"
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)

		if strings.HasPrefix(line, "Chipset Model:") && model == "Unknown" {
			model = strings.TrimSpace(strings.TrimPrefix(line, "Chipset Model:"))
		}
		if strings.HasPrefix(line, "Total Number of Cores:") {
			value := strings.TrimSpace(strings.TrimPrefix(line, "Total Number of Cores:"))
			if n, err := strconv.Atoi(value); err == nil {
				cores = n
			}
		}
		if strings.HasPrefix(line, "Vendor:") {
			value := strings.TrimSpace(strings.TrimPrefix(line, "Vendor:"))
			if strings.Contains(strings.ToLower(value), "apple") {
				vendor = "Apple"
			}
		}
		if strings.HasPrefix(line, "Metal Support:") {
			metal = strings.TrimSpace(strings.TrimPrefix(line, "Metal Support:"))
		}
	}
	return model, vendor, cores, metal
}

func isVirtualGPU(name, vendor string) bool {
	text := strings.ToLower(name + " " + vendor)
	virtualWords := []string{
		"mirage",
		"demoforge",
		"virtual",
		"remote display",
		"remote desktop",
		"microsoft basic display",
	}
	for _, word := range virtualWords {
		if strings.Contains(text, word) {
			return true
		}
	}
	return false
}

func normalizeGPUVendor(name, compatibility string) string {
	text := strings.ToLower(name + " " + compatibility)
	switch {
	case strings.Contains(text, "nvidia"):
		return "NVIDIA"
	case strings.Contains(text, "intel"):
		return "Intel"
	case strings.Contains(text, "advanced micro devices"),
		strings.Contains(text, "amd"),
		strings.Contains(text, "ati"):
		return "AMD"
	default:
		return compatibility
	}
}

func detectWindowsGPU() (string, string, int, string, string) {
	script := `
$g = @(Get-CimInstance Win32_VideoController |
    Select-Object Name,AdapterCompatibility,DriverVersion)
ConvertTo-Json -InputObject $g -Compress
`
	out, err := exec.Command("powershell.exe", "-NoProfile", "-Command", script).Output()
	if err != nil {
		return "Unknown", "", 0, "", ""
	}
	name, vendor, driver, ok := parseWindowsGPUJSON(out)
	if !ok {
		return "Unknown", "", 0, "", ""
	}
	return name, vendor, 0, driver, ""
}

// parseWindowsGPUJSON is the pure parsing half of detectWindowsGPU.
// Returns ok=false when no physical GPU is found.
func parseWindowsGPUJSON(data []byte) (name, vendor, driver string, ok bool) {
	var gpus []windowsGPU
	if err := json.Unmarshal(data, &gpus); err != nil {
		return "", "", "", false
	}
	for _, gpu := range gpus {
		if isVirtualGPU(gpu.Name, gpu.AdapterCompatibility) {
			continue
		}
		vendor := normalizeGPUVendor(gpu.Name, gpu.AdapterCompatibility)
		return gpu.Name, vendor, gpu.DriverVersion, true
	}
	return "", "", "", false
}
