package executor

import (
	"fmt"
	"os/exec"

	"github.com/go-logr/logr"
	networkingv1alpha1 "networking.med.io/vlan-traffic-control/api/v1alpha1"
)

// ApplyHtbHierarchy executes Linux tc commands to configure HTB + fq_codel with dynamic classifiers
func ApplyHtbHierarchy(spec *networkingv1alpha1.HtbRootSpec, log logr.Logger) error {
	if spec == nil {
		return fmt.Errorf("htbRoot spec is nil")
	}

	iface := spec.Interface

	// 1. Flush existing root and ingress qdiscs prior to setup
	_ = FlushInterface(iface)

	// Resolve Root Handle (e.g., 1 for handle 1:)
	rootHandle := spec.HtbID
	if rootHandle <= 0 {
		rootHandle = 1
	}

	// Resolve Default Class Minor (e.g., 99 for default 1:99)
	defaultMinor := spec.DefaultClassMinor
	if defaultMinor <= 0 {
		defaultMinor = 99
	}

	rootHandleStr := fmt.Sprintf("%d:", rootHandle)
	parentClassStr := fmt.Sprintf("%d:1", rootHandle)

	// 2. Attach root HTB qdisc
	cmdRoot := execHostCommand("tc", "qdisc", "add", "dev", iface, "root", "handle", rootHandleStr, "htb", "default", fmt.Sprintf("%d", defaultMinor))
	if out, err := cmdRoot.CombinedOutput(); err != nil {
		return fmt.Errorf("failed root htb on %s: %s (%v)", iface, string(out), err)
	}

	// 3. Create parent class (e.g., 1:1 or 2:1)
	cmdParent := execHostCommand("tc", "class", "add", "dev", iface, "parent", rootHandleStr, "classid", parentClassStr,
		"htb", "rate", spec.Rate, "ceil", spec.Rate, "burst", "0b", "cburst", "0b")
	if out, err := cmdParent.CombinedOutput(); err != nil {
		return fmt.Errorf("failed class %s on %s: %s (%v)", parentClassStr, iface, string(out), err)
	}

	// 3b. Create Default Fallback Class explicitly set to Priority 0
	defaultClassHandle := fmt.Sprintf("%d:%d", rootHandle, defaultMinor)
	cmdDefaultClass := execHostCommand("tc", "class", "add", "dev", iface, "parent", parentClassStr,
		"classid", defaultClassHandle, "htb", "prio", "0", "rate", "1Mbit", "ceil", spec.Rate)
	if out, err := cmdDefaultClass.CombinedOutput(); err != nil {
		log.Info("[HTB] Warning: Failed creating default class", "classHandle", defaultClassHandle, "output", string(out), "error", err.Error())
	} else {
		log.Info("[HTB] Created default fallback class with PRIO 0", "classHandle", defaultClassHandle)
	}

	// 4. Iterate and configure child classes, fq_codel, and classifiers
	for _, c := range spec.Classes {
		classHandle := fmt.Sprintf("%d:%d", rootHandle, c.ClassMinor)

		rate := c.EgressRate
		ceil := c.EgressCeil
		if ceil == "" {
			ceil = rate
		}

		burst := c.EgressBurst
		if burst == "" {
			burst = "1250b"
		}

		// A. Subclass HTB
		cmdClassArgs := []string{"tc", "class", "add", "dev", iface, "parent", parentClassStr,
			"classid", classHandle, "htb",
			"prio", fmt.Sprintf("%d", c.Priority),
			"rate", rate,
			"ceil", ceil,
			"burst", burst,
		}
		cmdClass := execHostCommand(cmdClassArgs[0], cmdClassArgs[1:]...)
		if out, err := cmdClass.CombinedOutput(); err != nil {
			return fmt.Errorf("failed class %s on %s: %s (%v)", classHandle, iface, string(out), err)
		}

		// B. Leaf qdisc fq_codel
		if c.EnableFqCodel {
			leafHandle := fmt.Sprintf("%d:", c.ClassMinor)
			cmdLeaf := execHostCommand("tc", "qdisc", "add", "dev", iface, "parent", classHandle,
				"handle", leafHandle, "fq_codel")
			_ = cmdLeaf.Run()
		}

		// C. Dynamic Egress Classification Filter (VLAN, Subnet, Mark, or Auto)
		_, proto, matchArgs, desc, err := ResolveClassifier(c, rootHandle)
		if err != nil {
			log.Info("[HTB] Warning: Skipping filter generation", "classHandle", classHandle, "reason", err.Error())
			continue
		}

		log.Info("[HTB] Adding egress filter rule", "interface", iface, "strategy", desc, "targetClass", classHandle)

		filterParentStr := fmt.Sprintf("%d:0", rootHandle)
		filterArgs := []string{"tc", "filter", "add", "dev", iface, "parent", filterParentStr,
			"protocol", proto, "prio", fmt.Sprintf("%d", c.Priority), "flower"}
		filterArgs = append(filterArgs, matchArgs...)
		filterArgs = append(filterArgs, "flowid", classHandle)

		cmdFilter := execHostCommand(filterArgs[0], filterArgs[1:]...)
		if out, err := cmdFilter.CombinedOutput(); err != nil {
			log.Info("[HTB] Warning: Failed adding egress filter", "class", classHandle, "output", string(out), "error", err.Error())
		}
	}

	return nil
}

// FlushInterface deletes root and ingress qdiscs from interface to wipe all TC rules
func FlushInterface(iface string) error {
	// 1. Delete root HTB qdisc (wipes all egress classes and egress filters)
	cmdRoot := execHostCommand("tc", "qdisc", "del", "dev", iface, "root")
	_ = cmdRoot.Run()

	// 2. Delete ingress qdisc (wipes all ingress policing filters)
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
