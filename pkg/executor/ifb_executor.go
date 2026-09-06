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

	cmdTxq := execHostCommand("ip", "link", "set", "dev", ifbName, "txqueuelen", "1000")
	if out, errTxq := cmdTxq.CombinedOutput(); errTxq != nil {
		log.Error(fmt.Errorf("%s", string(out)), "[IFB] Warning: failed setting txqueuelen on IFB device", "ifbDev", ifbName)
	} else {
		log.Info("✓ Set txqueuelen=1000 on IFB device", "ifbDev", ifbName)
	}

	cmdUp := execHostCommand("ip", "link", "set", "dev", ifbName, "up")
	if out, errUp := cmdUp.CombinedOutput(); errUp != nil {
		return "", fmt.Errorf("failed bringing UP IFB device %s: %s (%v)", ifbName, string(out), errUp)
	}

	cmdIngressQdisc := execHostCommand("tc", "qdisc", "add", "dev", physIface, "handle", "ffff:", "ingress")
	_ = cmdIngressQdisc.Run()

	// FLUSH STALE FILTERS: Clean up any old stateless policing rules on parent ffff:
	cmdFlushIngress := execHostCommand("tc", "filter", "del", "dev", physIface, "parent", "ffff:")
	_ = cmdFlushIngress.Run()

	// Add catch-all mirred redirect filter
	cmdRedirect := execHostCommand("tc", "filter", "add", "dev", physIface, "parent", "ffff:",
		"protocol", "all", "prio", "1", "handle", "1", "matchall",
		"action", "mirred", "egress", "redirect", "dev", ifbName)
	if out, errRedir := cmdRedirect.CombinedOutput(); errRedir != nil {
		return "", fmt.Errorf("failed setting ingress redirect from %s to %s: %s (%v)", physIface, ifbName, string(out), errRedir)
	}

	log.Info("✅ [IFB] Ingress redirect active", "physIface", physIface, "ifbDev", ifbName)
	return ifbName, nil
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
			if cls.IngressCeil != "" {
				cCopy.EgressCeil = cls.IngressCeil
			} else {
				cCopy.EgressCeil = physSpec.Rate
			}
			ingressClasses = append(ingressClasses, cCopy)
		}
	}
	ifbSpec.Classes = ingressClasses

	log.Info("[IFB-HTB] Applying ingress HTB + fq_codel shaping tree to IFB device", "ifbDev", ifbDev, "classCount", len(ifbSpec.Classes))
	return ApplyHtbHierarchy(&ifbSpec, log)
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
