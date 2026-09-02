package executor

import (
	"bufio"
	"fmt"
	"os/exec"
	"strings"

	"github.com/go-logr/logr"
	networkingv1alpha1 "networking.med.io/vlan-traffic-control/api/v1alpha1"
)

// ApplyHtbHierarchy executes Linux tc commands to configure HTB + fq_codel with dynamic classifiers
func ApplyHtbHierarchy(spec *networkingv1alpha1.HtbRootSpec, log logr.Logger) error {
	if spec == nil {
		log.Info("[HTB] Received nil spec, flushing interface")
		return nil
	}

	iface := spec.Interface
	if iface == "" {
		return fmt.Errorf("interface name is empty")
	}

	if len(spec.Classes) == 0 {
		log.Info("[HTB] No active classes targeted for node, flushing interface", "interface", iface)
		return FlushInterface(iface)
	}

	rootHandle := spec.HtbID
	if rootHandle <= 0 {
		rootHandle = 1
	}

	defaultMinor := spec.DefaultClassMinor
	if defaultMinor <= 0 {
		defaultMinor = 99
	}

	rootHandleStr := fmt.Sprintf("%d:", rootHandle)
	parentClassStr := fmt.Sprintf("%d:1", rootHandle)
	defaultClassHandle := fmt.Sprintf("%d:%d", rootHandle, defaultMinor)

	desiredClasses := make(map[string]bool)
	desiredClasses[parentClassStr] = true
	desiredClasses[defaultClassHandle] = true

	for _, c := range spec.Classes {
		var classHandle string
		if c.ClassID != "" {
			classHandle = c.ClassID
		} else {
			classHandle = fmt.Sprintf("%d:%d", rootHandle, c.ClassMinor)
		}
		desiredClasses[classHandle] = true
	}

	log.Info("[HTB] Running Orphan Class Prune Pass...", "interface", iface, "desiredClassCount", len(desiredClasses))
	PruneOrphanedClasses(iface, rootHandle, desiredClasses, log)

	// Attach root HTB qdisc
	cmdRoot := execHostCommand("tc", "qdisc", "add", "dev", iface, "root", "handle", rootHandleStr, "htb", "default", fmt.Sprintf("%d", defaultMinor))
	_ = cmdRoot.Run()

	// Parent class
	cmdParent := execHostCommand("tc", "class", "change", "dev", iface, "parent", rootHandleStr, "classid", parentClassStr,
		"htb", "rate", spec.Rate, "ceil", spec.Rate)
	if out, err := cmdParent.CombinedOutput(); err != nil {
		cmdParentAdd := execHostCommand("tc", "class", "add", "dev", iface, "parent", rootHandleStr, "classid", parentClassStr,
			"htb", "rate", spec.Rate, "ceil", spec.Rate)
		if outAdd, errAdd := cmdParentAdd.CombinedOutput(); errAdd != nil {
			log.Info("[HTB] Parent class setup warning", "class", parentClassStr, "output", string(outAdd), "error", errAdd.Error())
		} else {
			_ = out
		}
	}

	// Default fallback class
	cmdDefaultClass := execHostCommand("tc", "class", "change", "dev", iface, "parent", parentClassStr,
		"classid", defaultClassHandle, "htb", "prio", "0", "rate", "1Mbit", "ceil", spec.Rate)
	if out, err := cmdDefaultClass.CombinedOutput(); err != nil {
		cmdDefaultAdd := execHostCommand("tc", "class", "add", "dev", iface, "parent", parentClassStr,
			"classid", defaultClassHandle, "htb", "prio", "0", "rate", "1Mbit", "ceil", spec.Rate)
		_ = cmdDefaultAdd.Run()
		_ = out
	}

	// Child classes
	for _, c := range spec.Classes {
		var classHandle string
		if c.ClassID != "" {
			classHandle = c.ClassID
		} else {
			classHandle = fmt.Sprintf("%d:%d", rootHandle, c.ClassMinor)
		}

		rate := c.EgressRate
		ceil := c.EgressCeil
		if ceil == "" {
			ceil = rate
		}

		burst := c.EgressBurst
		if burst == "" {
			burst = "15k" // Safe default burst above single packet MTU
		}

		cmdClassArgs := []string{"tc", "class", "change", "dev", iface, "parent", parentClassStr,
			"classid", classHandle, "htb",
			"prio", fmt.Sprintf("%d", c.Priority),
			"rate", rate,
			"ceil", ceil,
			"burst", burst,
		}
		cmdClass := execHostCommand(cmdClassArgs[0], cmdClassArgs[1:]...)
		if out, err := cmdClass.CombinedOutput(); err != nil {
			cmdAddArgs := append([]string{"tc", "class", "add"}, cmdClassArgs[3:]...)
			cmdAdd := execHostCommand(cmdAddArgs[0], cmdAddArgs[1:]...)
			if outAdd, errAdd := cmdAdd.CombinedOutput(); errAdd != nil {
				return fmt.Errorf("failed class %s on %s: %s (change err: %s, add err: %v)", classHandle, iface, string(outAdd), string(out), errAdd)
			}
		}

		if c.EnableFqCodel {
			var classMinor int
			if c.ClassMinor > 0 {
				classMinor = c.ClassMinor
			} else {
				parts := strings.Split(classHandle, ":")
				if len(parts) == 2 {
					fmt.Sscanf(parts[1], "%d", &classMinor)
				}
			}
			leafHandle := fmt.Sprintf("%d:", classMinor)
			cmdLeaf := execHostCommand("tc", "qdisc", "add", "dev", iface, "parent", classHandle,
				"handle", leafHandle, "fq_codel")
			_ = cmdLeaf.Run()
		}

		proto, flowerMatch, desc, filterPrio, err := ResolveClassifier(c, rootHandle)
		if err != nil {
			log.Info("[HTB] Warning: Skipping filter generation", "classHandle", classHandle, "reason", err.Error())
			continue
		}

		log.Info("[HTB] Reconciling egress filter rule", "interface", iface, "strategy", desc, "targetClass", classHandle)

		filterParentStr := fmt.Sprintf("%d:0", rootHandle)
		
		var filterArgs []string
		// Handle 'fw' mark classification vs 'flower' classification CLI structure cleanly
		if proto == "all" && len(flowerMatch) >= 3 && flowerMatch[2] == "fw" {
			// fw classifier: tc filter replace dev <iface> parent 1:0 protocol all prio <prio> handle <mark> fw flowid 1:300
			filterArgs = []string{"tc", "filter", "replace", "dev", iface, "parent", filterParentStr,
				"protocol", proto, "prio", fmt.Sprintf("%d", filterPrio),
				flowerMatch[0], flowerMatch[1], flowerMatch[2], "flowid", classHandle}
		} else {
			// flower classifier: tc filter replace dev <iface> parent 1:0 protocol <proto> prio <prio> handle 1 flower <matches...> flowid 1:100
			// Explicitly passing 'handle 1' ensures atomic replacement without 'RTNETLINK answers: File exists'
			filterArgs = []string{"tc", "filter", "replace", "dev", iface, "parent", filterParentStr,
				"protocol", proto, "prio", fmt.Sprintf("%d", filterPrio), "handle", "1", "flower"}
			filterArgs = append(filterArgs, flowerMatch...)
			filterArgs = append(filterArgs, "flowid", classHandle)
		}

		cmdFilter := execHostCommand(filterArgs[0], filterArgs[1:]...)
		if out, err := cmdFilter.CombinedOutput(); err != nil {
			log.Info("[HTB] Warning: Filter replace failed, attempting preference flush and re-add", "class", classHandle, "output", string(out))

			// Fallback sequence: flush preference handle and re-add
			cmdDelFilter := execHostCommand("tc", "filter", "del", "dev", iface, "parent", filterParentStr, "prio", fmt.Sprintf("%d", filterPrio))
			_ = cmdDelFilter.Run()

			filterAddArgs := append([]string{"tc", "filter", "add"}, filterArgs[3:]...)
			cmdAddFilter := execHostCommand(filterAddArgs[0], filterAddArgs[1:]...)
			if outAdd, errAdd := cmdAddFilter.CombinedOutput(); errAdd != nil {
				log.Error(errAdd, "[HTB] Error: Failed adding egress filter after flush", "class", classHandle, "output", string(outAdd))
			}
		}
	}

	return nil
}

// PruneOrphanedClasses queries active HTB classes and deletes unreferenced ones safely
func PruneOrphanedClasses(iface string, rootHandle int, desiredClasses map[string]bool, log logr.Logger) {
	cmd := execHostCommand("tc", "class", "show", "dev", iface)
	out, err := cmd.Output()
	if err != nil {
		log.Info("[PRUNE] Unable to query tc classes (interface may not exist yet)", "interface", iface, "error", err.Error())
		return
	}

	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)

		if len(fields) >= 3 && fields[0] == "class" {
			classHandle := fields[2]

			prefix := fmt.Sprintf("%d:", rootHandle)
			if !strings.HasPrefix(classHandle, prefix) {
				continue
			}

			if !desiredClasses[classHandle] {
				log.Info("🗑️ [PRUNE] Removing orphaned TC class from kernel", "interface", iface, "classHandle", classHandle)

				// Selectively delete class (filters targeting this flowid can be safely replaced/flushed)
				cmdDelClass := execHostCommand("tc", "class", "del", "dev", iface, "classid", classHandle)
				if outDel, errDel := cmdDelClass.CombinedOutput(); errDel != nil {
					log.Info("⚠️ [PRUNE] Failed deleting orphaned class", "classHandle", classHandle, "output", string(outDel), "error", errDel.Error())
				} else {
					log.Info("✅ [PRUNE] Successfully deleted orphaned class", "classHandle", classHandle)
				}
			}
		}
	}
}

// FlushInterface deletes root and ingress qdiscs from interface to wipe all TC rules
func FlushInterface(iface string) error {
	cmdRoot := execHostCommand("tc", "qdisc", "del", "dev", iface, "root")
	_ = cmdRoot.Run()

	cmdIngress := execHostCommand("tc", "qdisc", "del", "dev", iface, "ingress")
	_ = cmdIngress.Run()

	return nil
}

// execHostCommand wraps command execution to run on node host namespace if chroot exists
func execHostCommand(name string, args ...string) *exec.Cmd {
	if _, err := exec.LookPath("chroot"); err == nil {
		fullArgs := append([]string{"/host", name}, args...)
		return exec.Command("chroot", fullArgs...)
	}
	return exec.Command(name, args...)
}
