package executor

import (
	"fmt"

	"github.com/go-logr/logr"
	networkingv1alpha1 "networking.med.io/vlan-traffic-control/api/v1alpha1"
)

// EnsureIfbDevice provisions an IFB device with multi-queue support and txqueuelen limit
func EnsureIfbDevice(physIface string, log logr.Logger) (string, error) {
	ifbName := fmt.Sprintf("ifb-%s", physIface)
	if len(ifbName) > 15 {
		ifbName = ifbName[:15]
	}

	_ = loadModule("ifb", "numtxqs=16")

	cmdCheck := execHostCommand("ip", "link", "show", ifbName)
	if err := cmdCheck.Run(); err != nil {
		log.Info("Creating IFB device for ingress shaping", "physIface", physIface, "ifbDev", ifbName)

		cmdAdd := execHostCommand("ip", "link", "add", ifbName, "type", "ifb")
		if out, errAdd := cmdAdd.CombinedOutput(); errAdd != nil {
			return "", fmt.Errorf("failed creating IFB device %s: %s (%v)", ifbName, string(out), errAdd)
		}
	}

	cmdTxq := execHostCommand("ip", "link", "set", "dev", ifbName, "txqueuelen", "10000")
	if out, errTxq := cmdTxq.CombinedOutput(); errTxq != nil {
		log.Error(fmt.Errorf("%s", string(out)), "[IFB] Warning: failed setting txqueuelen on IFB device", "ifbDev", ifbName)
	} else {
		log.Info("✓ Set txqueuelen=10000 on IFB device", "ifbDev", ifbName)
	}

	cmdUp := execHostCommand("ip", "link", "set", "dev", ifbName, "up")
	if out, errUp := cmdUp.CombinedOutput(); errUp != nil {
		return "", fmt.Errorf("failed bringing UP IFB device %s: %s (%v)", ifbName, string(out), errUp)
	}

	// 1. ENSURE CLSACT / INGRESS QDISC ON PHYSICAL INTERFACE
	cmdClsactQdisc := execHostCommand("tc", "qdisc", "add", "dev", physIface, "clsact")
	_ = cmdClsactQdisc.Run()

	cmdIngressQdisc := execHostCommand("tc", "qdisc", "add", "dev", physIface, "handle", "ffff:", "ingress")
	_ = cmdIngressQdisc.Run()

	// 2. FLUSH STALE STATELESS FILTERS
	cmdFlushIngress := execHostCommand("tc", "filter", "del", "dev", physIface, "parent", "ffff:")
	_ = cmdFlushIngress.Run()

	cmdFlushClsact := execHostCommand("tc", "filter", "del", "dev", physIface, "ingress")
	_ = cmdFlushClsact.Run()

	// 3. ATTACH CATCH-ALL MIRRED REDIRECT FILTER TO BOTH INGRESS AND CLSACT HANDLES
	cmdRedirectClsact := execHostCommand("tc", "filter", "add", "dev", physIface, "ingress",
		"protocol", "all", "prio", "1", "handle", "1", "matchall",
		"action", "mirred", "egress", "redirect", "dev", ifbName)
	if _, errRedir := cmdRedirectClsact.CombinedOutput(); errRedir != nil {
		cmdRedirectLegacy := execHostCommand("tc", "filter", "add", "dev", physIface, "parent", "ffff:",
			"protocol", "all", "prio", "1", "handle", "1", "matchall",
			"action", "mirred", "egress", "redirect", "dev", ifbName)
		if outLegacy, errLegacy := cmdRedirectLegacy.CombinedOutput(); errLegacy != nil {
			return "", fmt.Errorf("failed setting ingress redirect from %s to %s: %s (%v)", physIface, ifbName, string(outLegacy), errLegacy)
		}
	}

	log.Info("✅ [IFB] Ingress redirect active", "physIface", physIface, "ifbDev", ifbName)
	return ifbName, nil
}

// EnsureIfbIngressFilters creates flower classification rules on IFB device to direct packets to target HTB classes
func EnsureIfbIngressFilters(ifbDev string, spec *networkingv1alpha1.HtbRootSpec, log logr.Logger) error {
	rootHandle := spec.HtbID
	if rootHandle <= 0 {
		rootHandle = 1
	}

	// Ensure ingress qdisc on IFB device for filter placement
	_ = execHostCommand("tc", "qdisc", "add", "dev", ifbDev, "ingress").Run()
	_ = execHostCommand("tc", "filter", "del", "dev", ifbDev, "ingress").Run()

	for idx, cls := range spec.Classes {
		if cls.IngressRate == "" {
			continue
		}

		prio := cls.Priority
		if prio <= 0 {
			prio = idx + 1
		}

		classID := cls.GetClassID(rootHandle)

		if cls.VlanID > 0 {
			vlanStr := fmt.Sprintf("%d", cls.VlanID)
			cmdFlower := execHostCommand("tc", "filter", "add", "dev", ifbDev, "ingress",
				"protocol", "802.1q", "prio", fmt.Sprintf("%d", prio), "handle", "1", "flower",
				"vlan_id", vlanStr,
				"classid", classID)
			if _, err := cmdFlower.CombinedOutput(); err != nil {
				// Fallback to skbedit priority if classid action is rejected by kernel netlink
				cmdFallback := execHostCommand("tc", "filter", "add", "dev", ifbDev, "ingress",
					"protocol", "802.1q", "prio", fmt.Sprintf("%d", prio), "handle", "1", "flower",
					"vlan_id", vlanStr,
					"action", "skbedit", "priority", classID)
				_, _ = cmdFallback.CombinedOutput()
			}
		} else if cls.Subnet != "" {
			cmdSubnet := execHostCommand("tc", "filter", "add", "dev", ifbDev, "ingress",
				"protocol", "ip", "prio", fmt.Sprintf("%d", prio), "handle", "1", "flower",
				"dst_ip", cls.Subnet,
				"classid", classID)
			if _, err := cmdSubnet.CombinedOutput(); err != nil {
				cmdFallback := execHostCommand("tc", "filter", "add", "dev", ifbDev, "ingress",
					"protocol", "ip", "prio", fmt.Sprintf("%d", prio), "handle", "1", "flower",
					"dst_ip", cls.Subnet,
					"action", "skbedit", "priority", classID)
				_, _ = cmdFallback.CombinedOutput()
			}
		}
	}
	return nil
}

// ReconcileIngressHtb configures IFB redirection and applies HTB hierarchy + fq_codel leaf qdiscs
func ReconcileIngressHtb(physSpec *networkingv1alpha1.HtbRootSpec, log logr.Logger) error {
	if physSpec == nil || physSpec.Interface == "" {
		return nil
	}

	physIface := physSpec.Interface

	ifbDev, err := EnsureIfbDevice(physIface, log)
	if err != nil {
		return err
	}

	ifbSpec := *physSpec
	ifbSpec.Interface = ifbDev

	var ingressClasses []networkingv1alpha1.ClassSpec
	for _, cls := range physSpec.Classes {
		if cls.IngressRate != "" {
			cCopy := cls
			cCopy.EnableFqCodel = true
			cCopy.EgressRate = cls.IngressRate
			cCopy.IngressRate = cls.IngressRate
			if cls.IngressCeil != "" {
				cCopy.EgressCeil = cls.IngressCeil
				cCopy.IngressCeil = cls.IngressCeil
			} else {
				cCopy.EgressCeil = physSpec.Rate
				cCopy.IngressCeil = physSpec.Rate
			}
			ingressClasses = append(ingressClasses, cCopy)
		}
	}
	ifbSpec.Classes = ingressClasses

	log.Info("[IFB-HTB] Applying ingress HTB + fq_codel shaping tree to IFB device", "ifbDev", ifbDev, "classCount", len(ifbSpec.Classes))
	if errHtb := ApplyHtbHierarchy(&ifbSpec, log); errHtb != nil {
		return errHtb
	}

	return EnsureIfbIngressFilters(ifbDev, &ifbSpec, log)
}

// FlushIfbDevice cleanly tears down and removes the virtual IFB device for an interface
func FlushIfbDevice(physIface string) error {
	ifbName := fmt.Sprintf("ifb-%s", physIface)
	if len(ifbName) > 15 {
		ifbName = ifbName[:15]
	}

	cmdDel := execHostCommand("ip", "link", "del", "dev", ifbName)
	return cmdDel.Run()
}
