// Package hardware detects the host's compute capabilities: CPU features,
// GPUs, and NPUs. NexusRun is NPU-first: detection is structured so an NPU,
// when present, is a first-class scheduling target rather than an afterthought.
package hardware

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Accelerator classes, in the runtime's default preference order.
const (
	ClassNPU = "npu"
	ClassGPU = "gpu"
	ClassCPU = "cpu"
)

// Device is one detected compute device.
type Device struct {
	Class    string `json:"class"`  // npu | gpu | cpu
	Vendor   string `json:"vendor"` // intel, amd, nvidia, apple, qualcomm…
	Name     string `json:"name"`
	MemoryMB int64  `json:"memory_mb,omitempty"` // dedicated memory when known
	Driver   string `json:"driver,omitempty"`
	// Backend hints which execution backend can drive this device,
	// e.g. "llama.cpp/cuda", "onnxruntime/openvino", "onnxruntime/qnn".
	Backend string `json:"backend,omitempty"`
}

// Report is the full detection result.
type Report struct {
	OS          string   `json:"os"`
	Arch        string   `json:"arch"`
	CPUModel    string   `json:"cpu_model"`
	CPUCores    int      `json:"cpu_cores"`
	CPUFeatures []string `json:"cpu_features,omitempty"` // avx2, avx512, neon…
	TotalRAMMB  int64    `json:"total_ram_mb"`
	Devices     []Device `json:"devices"`
}

// Detect probes the host. It never fails hard: partial information is
// better than none on exotic hardware.
func Detect() *Report {
	r := &Report{
		OS:       runtime.GOOS,
		Arch:     runtime.GOARCH,
		CPUCores: runtime.NumCPU(),
	}
	detectCPU(r)
	detectRAM(r)
	detectNPU(r)
	detectGPU(r)
	// CPU is always a device of last resort.
	r.Devices = append(r.Devices, Device{
		Class:   ClassCPU,
		Vendor:  cpuVendor(r.CPUModel),
		Name:    r.CPUModel,
		Backend: "llama.cpp/cpu",
	})
	return r
}

// Best returns the first detected device matching the preference order.
func (r *Report) Best(prefer []string) Device {
	for _, class := range prefer {
		for _, d := range r.Devices {
			if d.Class == class {
				return d
			}
		}
	}
	return r.Devices[len(r.Devices)-1] // CPU fallback, always present
}

// Has reports whether any device of the given class exists.
func (r *Report) Has(class string) bool {
	for _, d := range r.Devices {
		if d.Class == class {
			return true
		}
	}
	return false
}

func (r *Report) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s/%s · %s (%d cores", r.OS, r.Arch, r.CPUModel, r.CPUCores)
	if len(r.CPUFeatures) > 0 {
		fmt.Fprintf(&b, ", %s", strings.Join(r.CPUFeatures, "/"))
	}
	fmt.Fprintf(&b, ") · %.1f GB RAM\n", float64(r.TotalRAMMB)/1024)
	for _, d := range r.Devices {
		fmt.Fprintf(&b, "  [%s] %s %s", strings.ToUpper(d.Class), d.Vendor, d.Name)
		if d.MemoryMB > 0 {
			fmt.Fprintf(&b, " (%.1f GB)", float64(d.MemoryMB)/1024)
		}
		if d.Backend != "" {
			fmt.Fprintf(&b, " → %s", d.Backend)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func detectCPU(r *Report) {
	switch runtime.GOOS {
	case "linux":
		data, err := os.ReadFile("/proc/cpuinfo")
		if err != nil {
			return
		}
		for _, line := range strings.Split(string(data), "\n") {
			if r.CPUModel == "" && strings.HasPrefix(line, "model name") {
				if _, val, ok := strings.Cut(line, ":"); ok {
					r.CPUModel = strings.TrimSpace(val)
				}
			}
			if len(r.CPUFeatures) == 0 && strings.HasPrefix(line, "flags") {
				for _, want := range []string{"avx512f", "avx2", "avx", "sse4_2", "amx_tile"} {
					if strings.Contains(line, " "+want) {
						r.CPUFeatures = append(r.CPUFeatures, strings.ReplaceAll(want, "_", ""))
					}
				}
			}
		}
	case "darwin":
		if out, err := exec.Command("sysctl", "-n", "machdep.cpu.brand_string").Output(); err == nil {
			r.CPUModel = strings.TrimSpace(string(out))
		}
		if runtime.GOARCH == "arm64" {
			r.CPUFeatures = append(r.CPUFeatures, "neon")
		}
	default:
		r.CPUModel = runtime.GOARCH
	}
	if r.CPUModel == "" {
		r.CPUModel = runtime.GOARCH
	}
	if runtime.GOARCH == "arm64" && len(r.CPUFeatures) == 0 {
		r.CPUFeatures = append(r.CPUFeatures, "neon")
	}
}

func detectRAM(r *Report) {
	switch runtime.GOOS {
	case "linux":
		data, err := os.ReadFile("/proc/meminfo")
		if err != nil {
			return
		}
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "MemTotal:") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					kb, _ := strconv.ParseInt(fields[1], 10, 64)
					r.TotalRAMMB = kb / 1024
				}
				return
			}
		}
	case "darwin":
		if out, err := exec.Command("sysctl", "-n", "hw.memsize").Output(); err == nil {
			bytes, _ := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
			r.TotalRAMMB = bytes / (1024 * 1024)
		}
	}
}

// detectNPU probes for neural processing units. On Linux, NPUs surface
// through the kernel "accel" subsystem (/sys/class/accel); the bound
// driver identifies the vendor. On Apple Silicon the ANE is always
// present. Windows detection (QNN/OpenVINO device enumeration) runs
// through the ONNX Runtime backend at execution time.
func detectNPU(r *Report) {
	switch runtime.GOOS {
	case "linux":
		entries, err := filepath.Glob("/sys/class/accel/accel*")
		if err != nil {
			return
		}
		for _, e := range entries {
			driver := ""
			if link, err := os.Readlink(filepath.Join(e, "device/driver")); err == nil {
				driver = filepath.Base(link)
			}
			d := Device{Class: ClassNPU, Driver: driver}
			switch driver {
			case "intel_vpu":
				d.Vendor, d.Name, d.Backend = "intel", "Intel NPU (Core Ultra)", "onnxruntime/openvino"
			case "amdxdna":
				d.Vendor, d.Name, d.Backend = "amd", "AMD XDNA (Ryzen AI)", "onnxruntime/vitisai"
			case "qaic":
				d.Vendor, d.Name, d.Backend = "qualcomm", "Qualcomm Cloud AI", "onnxruntime/qnn"
			case "rocket":
				d.Vendor, d.Name, d.Backend = "rockchip", "Rockchip NPU", "rknn"
			default:
				d.Vendor, d.Name, d.Backend = "unknown", "accel device ("+driver+")", "onnxruntime"
			}
			r.Devices = append(r.Devices, d)
		}
	case "darwin":
		if runtime.GOARCH == "arm64" {
			r.Devices = append(r.Devices, Device{
				Class: ClassNPU, Vendor: "apple", Name: "Apple Neural Engine",
				Backend: "onnxruntime/coreml",
			})
		}
	}
}

func detectGPU(r *Report) {
	// NVIDIA: nvidia-smi is the reliable cross-distro signal.
	if out, err := exec.Command("nvidia-smi", "--query-gpu=name,memory.total", "--format=csv,noheader,nounits").Output(); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			name, mem, _ := strings.Cut(line, ",")
			memMB, _ := strconv.ParseInt(strings.TrimSpace(mem), 10, 64)
			if name = strings.TrimSpace(name); name != "" {
				r.Devices = append(r.Devices, Device{
					Class: ClassGPU, Vendor: "nvidia", Name: name,
					MemoryMB: memMB, Driver: "nvidia", Backend: "llama.cpp/cuda",
				})
			}
		}
		return
	}
	switch runtime.GOOS {
	case "darwin":
		if runtime.GOARCH == "arm64" {
			r.Devices = append(r.Devices, Device{
				Class: ClassGPU, Vendor: "apple", Name: "Apple Silicon GPU",
				Backend: "llama.cpp/metal",
			})
		}
	case "linux":
		// AMD/Intel GPUs expose render nodes; identify vendor via driver link.
		nodes, _ := filepath.Glob("/sys/class/drm/renderD*/device/driver")
		for _, n := range nodes {
			link, err := os.Readlink(n)
			if err != nil {
				continue
			}
			switch filepath.Base(link) {
			case "amdgpu":
				r.Devices = append(r.Devices, Device{
					Class: ClassGPU, Vendor: "amd", Name: "AMD GPU (amdgpu)",
					Driver: "amdgpu", Backend: "llama.cpp/rocm",
				})
			case "i915", "xe":
				r.Devices = append(r.Devices, Device{
					Class: ClassGPU, Vendor: "intel", Name: "Intel GPU (" + filepath.Base(link) + ")",
					Driver: filepath.Base(link), Backend: "llama.cpp/vulkan",
				})
			}
		}
	}
}

func cpuVendor(model string) string {
	l := strings.ToLower(model)
	switch {
	case strings.Contains(l, "intel"):
		return "intel"
	case strings.Contains(l, "amd"):
		return "amd"
	case strings.Contains(l, "apple"):
		return "apple"
	default:
		return "generic"
	}
}
