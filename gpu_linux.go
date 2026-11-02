package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// GPUs without nvidia-smi (the laptop's Radeon 780M, an AMD eGPU, Intel) read from /sys.

type sysGPU struct {
	name, driver        string
	vram, vramUsed, gtt int64
	gttUsed             int64
	busy                int // percent, -1 unknown
	tempC               float64
}

func sysInt(p string) (int64, bool) {
	b, err := os.ReadFile(p)
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return n, err == nil
}

var gpuVendors = map[string]string{"0x1002": "AMD", "0x8086": "Intel", "0x10de": "NVIDIA"}

func sysGPUs() []sysGPU {
	cards, _ := filepath.Glob("/sys/class/drm/card[0-9]*")
	var out []sysGPU
	for _, c := range cards {
		if strings.Contains(filepath.Base(c), "-") { // connectors (card1-HDMI-A-1)
			continue
		}
		d := filepath.Join(c, "device")
		vb, err := os.ReadFile(filepath.Join(d, "vendor"))
		if err != nil {
			continue
		}
		g := sysGPU{busy: -1}
		vendor := gpuVendors[strings.TrimSpace(string(vb))]
		if drv, err := os.Readlink(filepath.Join(d, "driver")); err == nil {
			g.driver = filepath.Base(drv)
		}
		g.name = gpuName(d, vendor)
		g.vram, _ = sysInt(filepath.Join(d, "mem_info_vram_total"))
		g.vramUsed, _ = sysInt(filepath.Join(d, "mem_info_vram_used"))
		g.gtt, _ = sysInt(filepath.Join(d, "mem_info_gtt_total"))
		g.gttUsed, _ = sysInt(filepath.Join(d, "mem_info_gtt_used"))
		if b, ok := sysInt(filepath.Join(d, "gpu_busy_percent")); ok {
			g.busy = int(b)
		}
		if ts, _ := filepath.Glob(filepath.Join(d, "hwmon", "hwmon*", "temp1_input")); len(ts) > 0 {
			if t, ok := sysInt(ts[0]); ok {
				g.tempC = float64(t) / 1000
			}
		}
		out = append(out, g)
	}
	return out
}

// gpuName: lspci's name for the card, else vendor + device id.
func gpuName(dev, vendor string) string {
	if b, err := os.ReadFile(filepath.Join(dev, "uevent")); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if slot, ok := strings.CutPrefix(l, "PCI_SLOT_NAME="); ok {
				if out, err := runQuiet(5*time.Second, "lspci", "-s", slot); err == nil {
					if _, name, ok := strings.Cut(strings.TrimSpace(out), ": "); ok {
						return name
					}
				}
			}
		}
	}
	id, _ := os.ReadFile(filepath.Join(dev, "device"))
	return strings.TrimSpace(vendor + " GPU " + strings.TrimSpace(string(id)))
}

func (g sysGPU) String() string {
	s := g.name
	if g.driver != "" {
		s += ", driver " + g.driver
	}
	if g.vram > 0 {
		s += fmt.Sprintf(", %s VRAM (%s used)", hb(g.vram), hb(g.vramUsed))
	}
	if g.gtt > 0 {
		s += fmt.Sprintf(", %s shared (%s used)", hb(g.gtt), hb(g.gttUsed))
	}
	if g.busy >= 0 {
		s += fmt.Sprintf(", %d%% busy", g.busy)
	}
	if g.tempC > 0 {
		s += fmt.Sprintf(", %.0f°C", g.tempC)
	}
	return s
}

// sysGPUInfo: one line for system info ("" when no GPU is visible).
func sysGPUInfo() string {
	var gs []string
	for _, g := range sysGPUs() {
		gs = append(gs, g.String())
	}
	return strings.Join(gs, " | ")
}

// sysGPUBusy: a GPU doing real work (busy, or a quarter of its VRAM in use).
func sysGPUBusy() string {
	for _, g := range sysGPUs() {
		if g.vram > 0 && (g.vramUsed > g.vram/4 || g.busy >= 40) {
			return fmt.Sprintf("GPU %s has %.1f of %.1f GB VRAM in use and is %d%% busy", g.name, float64(g.vramUsed)/(1<<30), float64(g.vram)/(1<<30), max(g.busy, 0))
		}
	}
	return ""
}
