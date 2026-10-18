package hardware

import (
	"testing"
)

const sampleMacOutput = `
Graphics/Displays:

    Apple M3 Pro:

      Chipset Model: Apple M3 Pro
      Type: GPU
      Bus: Built-In
      Total Number of Cores: 18
      Vendor: Apple (0x106b)
      Metal Support: Metal 3
`

func TestParseMacGPU(t *testing.T) {
	model, vendor, cores, metal := parseMacGPUOutput(sampleMacOutput)
	if model != "Apple M3 Pro" {
		t.Fatalf("model: got %q", model)
	}
	if vendor != "Apple" {
		t.Fatalf("vendor: got %q", vendor)
	}
	if cores != 18 {
		t.Fatalf("cores: got %d", cores)
	}
	if metal != "Metal 3" {
		t.Fatalf("metal: got %q", metal)
	}
}

func TestParseMacGPUEmpty(t *testing.T) {
	model, _, cores, _ := parseMacGPUOutput("")
	if model != "Unknown" || cores != 0 {
		t.Fatalf("empty output should yield Unknown/0, got %q/%d", model, cores)
	}
}

const sampleWindowsGPUs = `[
  {"Name":"Microsoft Basic Display Adapter","AdapterCompatibility":"Microsoft","DriverVersion":"10.0"},
  {"Name":"NVIDIA GeForce RTX 4070","AdapterCompatibility":"NVIDIA","DriverVersion":"31.0.15.5"}
]`

func TestParseWindowsGPUSkipsVirtual(t *testing.T) {
	name, vendor, driver, ok := parseWindowsGPUJSON([]byte(sampleWindowsGPUs))
	if !ok {
		t.Fatal("expected a physical GPU to be found")
	}
	if name != "NVIDIA GeForce RTX 4070" {
		t.Fatalf("name: got %q", name)
	}
	if vendor != "NVIDIA" {
		t.Fatalf("vendor: got %q", vendor)
	}
	if driver != "31.0.15.5" {
		t.Fatalf("driver: got %q", driver)
	}
}

func TestParseWindowsGPUAllVirtual(t *testing.T) {
	data := `[{"Name":"Remote Display Adapter","AdapterCompatibility":"DemoForge","DriverVersion":"1.0"}]`
	if _, _, _, ok := parseWindowsGPUJSON([]byte(data)); ok {
		t.Fatal("expected no GPU when all are virtual")
	}
}

func TestParseWindowsGPUInvalidJSON(t *testing.T) {
	if _, _, _, ok := parseWindowsGPUJSON([]byte("not json")); ok {
		t.Fatal("expected failure on invalid JSON")
	}
}

func TestNormalizeGPUVendor(t *testing.T) {
	cases := []struct{ name, compat, want string }{
		{"NVIDIA GeForce RTX 4070", "NVIDIA", "NVIDIA"},
		{"Intel Iris Xe", "Intel Corporation", "Intel"},
		{"AMD Radeon RX 7900", "Advanced Micro Devices, Inc.", "AMD"},
		{"ATI Radeon HD", "ATI", "AMD"},
		{"Some Weird GPU", "WeirdCorp", "WeirdCorp"},
	}
	for _, c := range cases {
		if got := normalizeGPUVendor(c.name, c.compat); got != c.want {
			t.Errorf("normalizeGPUVendor(%q): got %q want %q", c.name, got, c.want)
		}
	}
}

func TestIsVirtualGPU(t *testing.T) {
	if !isVirtualGPU("Mirage Driver", "") {
		t.Error("mirage should be virtual")
	}
	if !isVirtualGPU("Microsoft Basic Display Adapter", "Microsoft") {
		t.Error("basic display should be virtual")
	}
	if isVirtualGPU("NVIDIA GeForce RTX 4070", "NVIDIA") {
		t.Error("real GPU flagged virtual")
	}
}

func TestDetectHardwareSmoke(t *testing.T) {
	model, physical, logical, ram := DetectHardware()
	if model == "" || model == "Unknown" {
		t.Logf("warning: cpu model unknown on this box")
	}
	if logical <= 0 || physical <= 0 {
		t.Fatalf("cores must be positive: %d/%d", physical, logical)
	}
	if ram <= 0 {
		t.Fatalf("ram must be positive: %f", ram)
	}
}

func TestDetectStorageSmoke(t *testing.T) {
	total, used, free := DetectStorage()
	if total <= 0 {
		t.Fatalf("total must be positive: %f", total)
	}
	if used < 0 || free < 0 {
		t.Fatalf("used/free must be non-negative: %f/%f", used, free)
	}
}
