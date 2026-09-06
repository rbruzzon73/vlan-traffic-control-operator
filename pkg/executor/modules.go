package executor

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"

	"github.com/go-logr/logr"
)

// EnsureKernelModulesLoaded verifies required kernel modules and loads missing ones.
func EnsureKernelModulesLoaded(strategy string, log logr.Logger) error {
	modules := []string{"sch_htb", "sch_fq_codel", "act_mirred"}

	switch strategy {
	case "flower":
		modules = append(modules, "cls_flower")
	case "u32", "auto", "ifb":
		modules = append(modules, "cls_u32", "cls_flower")
	}

	for _, mod := range modules {
		if !isModuleLoaded(mod) {
			log.Info("Kernel module not loaded. Attempting modprobe", "module", mod)
			// Pass numtxqs=16 specifically when loading the ifb module
			var opts []string
			if mod == "ifb" {
				opts = append(opts, "numtxqs=16")
			}
			if err := loadModule(mod, opts...); err != nil {
				return fmt.Errorf("failed to load required kernel module %s: %w", mod, err)
			}
			log.Info("Successfully loaded kernel module", "module", mod, "options", opts)
		} else {
			log.V(1).Info("Kernel module already loaded", "module", mod)
		}
	}
	return nil
}

func isModuleLoaded(mod string) bool {
	// Execute chroot /host /usr/sbin/lsmod
	cmd := exec.Command("/host/usr/sbin/chroot", "/host", "/usr/sbin/lsmod")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err == nil {
		return strings.Contains(out.String(), mod)
	}

	// Fallback trying standard host PATH
	cmdFallback := exec.Command("/host/usr/sbin/chroot", "/host", "lsmod")
	out.Reset()
	cmdFallback.Stdout = &out
	if err := cmdFallback.Run(); err == nil {
		return strings.Contains(out.String(), mod)
	}

	return false
}

func loadModule(mod string, params ...string) error {
	var errs []string

	argsAbs := append([]string{"/host", "/usr/sbin/modprobe", mod}, params...)
	cmdChrootAbs := exec.Command("/host/usr/sbin/chroot", argsAbs...)
	var stderrChrootAbs bytes.Buffer
	cmdChrootAbs.Stderr = &stderrChrootAbs
	if err := cmdChrootAbs.Run(); err == nil {
		return nil
	}
	errs = append(errs, fmt.Sprintf("chroot /usr/sbin/modprobe failed: %s", strings.TrimSpace(stderrChrootAbs.String())))

	argsSbin := append([]string{"/host", "/sbin/modprobe", mod}, params...)
	cmdChrootSbin := exec.Command("/host/usr/sbin/chroot", argsSbin...)
	var stderrChrootSbin bytes.Buffer
	cmdChrootSbin.Stderr = &stderrChrootSbin
	if err := cmdChrootSbin.Run(); err == nil {
		return nil
	}
	errs = append(errs, fmt.Sprintf("chroot /sbin/modprobe failed: %s", strings.TrimSpace(stderrChrootSbin.String())))

	return fmt.Errorf("modprobe %s failed across all attempts: %s", mod, strings.Join(errs, " | "))
}

func DetectHardwareCapabilities(iface string, log logr.Logger) (isHwOffload bool, isHighSpeed bool, isSRIOV bool) {
	cmd := exec.Command("/host/usr/sbin/chroot", "/host", "/usr/sbin/ethtool", "-k", iface)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err == nil {
		if strings.Contains(out.String(), "hw-tc-offload: on") {
			isHwOffload = true
		}
	}

	cmdSpeed := exec.Command("/host/usr/sbin/chroot", "/host", "/usr/sbin/ethtool", iface)
	out.Reset()
	cmdSpeed.Stdout = &out
	if err := cmdSpeed.Run(); err == nil {
		if strings.Contains(out.String(), "Speed: 100000Mb/s") || strings.Contains(out.String(), "Speed: 200000Mb/s") {
			isHighSpeed = true
		}
	}

	log.Info("Hardware detection completed", "interface", iface, "hwOffload", isHwOffload, "highSpeed100G", isHighSpeed)
	return
}
