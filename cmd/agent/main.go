package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"go.uber.org/zap"

	networkingv1alpha1 "networking.med.io/vlan-traffic-control/api/v1alpha1"
	"networking.med.io/vlan-traffic-control/pkg/executor"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	scheme = runtime.NewScheme()
)

func init() {
	_ = clientgoscheme.AddToScheme(scheme)
	_ = networkingv1alpha1.AddToScheme(scheme)
}

func main() {
	zapLog, err := zap.NewDevelopment()
	if err != nil {
		fmt.Printf("Failed to initialize logger: %v\n", err)
		os.Exit(1)
	}

	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		nodeName, _ = os.Hostname()
	}

	// Attach nodeName to base logger so ALL log entries automatically include node context
	log := zapr.NewLogger(zapLog).WithValues("nodeName", nodeName)

	log.Info("=== Starting VLAN Traffic Control Agent ===", "pid", os.Getpid())

	// 1. Ensure Kernel Modules are loaded
	log.Info("[BOOT] Verifying kernel modules (sch_htb, cls_flower, act_police)...")
	if err := executor.EnsureKernelModulesLoaded("auto", log); err != nil {
		log.Error(err, "[BOOT] Warning/Error ensuring kernel modules on host node")
	} else {
		log.Info("[BOOT] Kernel modules verified successfully")
	}

	// 2. Initialize Kubernetes client
	config, err := ctrl.GetConfig()
	if err != nil {
		log.Error(err, "[INIT] Failed to get Kubernetes in-cluster config")
		os.Exit(1)
	}

	k8sClient, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		log.Error(err, "[INIT] Failed to create Kubernetes client")
		os.Exit(1)
	}

	// 3. Initial reconciliation pass on startup
	log.Info("[INIT] Running startup TC reconciliation pass...")
	reconcileLocalTc(k8sClient, log)

	// 4. HTTP /stats Handler
	http.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		iface := r.URL.Query().Get("interface")
		if iface == "" {
			iface = "enp1s0"
		}

		log.Info("[API] GET /stats", "clientIP", r.RemoteAddr, "interface", iface)

		classStats, errClass := executor.GetHtbClassStatsStructured(iface)
		if errClass != nil {
			log.Error(errClass, "[API] Failed retrieving HTB class stats", "interface", iface)
		}

		ingressStats, errIngress := executor.GetIngressFilterStatsStructured(iface)
		if errIngress != nil {
			log.Error(errIngress, "[API] Failed retrieving ingress filter stats", "interface", iface)
		}

		log.Info("[API] GET /stats completed", "interface", iface, "classesFound", len(classStats), "ingressRulesFound", len(ingressStats))

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"node":         nodeName,
			"interface":    iface,
			"classStats":   classStats,
			"ingressStats": ingressStats,
		})
	})

	// 5. HTTP /cleanup Handler (Selective Rule Wipe)
	http.HandleFunc("/cleanup", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete && r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		iface := r.URL.Query().Get("interface")
		if iface == "" {
			iface = "enp1s0"
		}

		log.Info("========================================================================")
		log.Info("[CLEANUP] Starting selective TC rule cleanup...", "interface", iface, "triggeredBy", r.RemoteAddr)

		cleanedClasses, cleanedFilters := performSelectiveCleanup(k8sClient, iface, log)

		log.Info("[CLEANUP] Selective cleanup complete",
			"interface", iface,
			"classesRemoved", cleanedClasses,
			"filtersRemoved", cleanedFilters,
		)
		log.Info("========================================================================")

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"status":           "selective_cleanup_completed",
			"node":             nodeName,
			"interface":        iface,
			"classesRemoved": cleanedClasses,
			"filtersRemoved": cleanedFilters,
		})
	})

	// 6. HTTP /reconcile Handler
	http.HandleFunc("/reconcile", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		log.Info("[API] POST /reconcile triggered by Manager", "client", r.RemoteAddr)
		reconcileLocalTc(k8sClient, log)

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"reconciled","node":"` + nodeName + `"}`))
	})

	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	log.Info("[HTTP] Agent HTTP server listening", "port", 8080)
	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Error(err, "[FATAL] Agent HTTP server stopped unexpectedly")
		os.Exit(1)
	}
}

func reconcileLocalTc(k8sClient client.Client, log logr.Logger) {
	ctx := context.Background()
	var list networkingv1alpha1.VlanTrafficControlList

	if err := k8sClient.List(ctx, &list); err != nil {
		log.Error(err, "[RECONCILE] Failed listing VlanTrafficControl CRDs")
		return
	}

	if len(list.Items) == 0 {
		log.Info("[RECONCILE] No VlanTrafficControl CRD resources found on cluster")
		return
	}

	for _, item := range list.Items {
		log.Info("----------------------------------------------------------------")
		log.Info("[RECONCILE] Processing CRD instance", "name", item.Name, "interface", item.Spec.HtbRoot.Interface, "classCount", len(item.Spec.HtbRoot.Classes))

		strategyUsed, err := executor.ApplyAdaptiveHtbHierarchy(&item.Spec.HtbRoot, item.Spec.TcStrategy, log)
		if err != nil {
			log.Error(err, "[RECONCILE] Failed applying TC rules", "instance", item.Name, "strategy", strategyUsed)
		} else {
			log.Info("[RECONCILE] Successfully applied TC rules on host interface", "instance", item.Name, "interface", item.Spec.HtbRoot.Interface, "strategy", strategyUsed)
		}
		log.Info("----------------------------------------------------------------")
	}
}

// performSelectiveCleanup removes ONLY the specific classes and ingress/egress filters defined by the operator
func performSelectiveCleanup(k8sClient client.Client, iface string, log logr.Logger) (int, int) {
	ctx := context.Background()
	var list networkingv1alpha1.VlanTrafficControlList
	classesRemoved := 0
	filtersRemoved := 0

	if err := k8sClient.List(ctx, &list); err != nil {
		log.Error(err, "[CLEANUP] Failed to fetch CRD list for selective wipe")
		return 0, 0
	}

	for _, crd := range list.Items {
		if crd.Spec.HtbRoot.Interface != iface && iface != "all" {
			continue
		}

		targetIface := crd.Spec.HtbRoot.Interface
		rootHandle := crd.Spec.HtbRoot.HtbID
		if rootHandle <= 0 {
			rootHandle = 1
		}

		filterParentStr := fmt.Sprintf("%d:", rootHandle)

		for _, cls := range crd.Spec.HtbRoot.Classes {
			classHandle := fmt.Sprintf("%d:%d", rootHandle, cls.ClassMinor)

			proto, _, desc, errRes := executor.ResolveClassifier(cls, rootHandle)
			if errRes != nil {
				log.Info("[SELECTIVE-CLEANUP] Skipping filter delete due to invalid classifier", "classHandle", classHandle, "reason", errRes.Error())
			} else {
				// 1. Remove Egress Filter for this specific class
				cmdDelFilterEgress := []string{"chroot", "/host", "tc", "filter", "del", "dev", targetIface, "parent", filterParentStr, "protocol", proto}
				outFilter, errFilter := exec.Command(cmdDelFilterEgress[0], cmdDelFilterEgress[1:]...).CombinedOutput()
				if errFilter == nil {
					filtersRemoved++
					log.Info("[SELECTIVE-CLEANUP] Removed egress filter", "interface", targetIface, "strategy", desc, "targetClass", classHandle)
				} else {
					log.Info("[SELECTIVE-CLEANUP] Egress filter note", "interface", targetIface, "strategy", desc, "details", strings.TrimSpace(string(outFilter)))
				}

				// 2. Remove Ingress Policing Filter for this specific class
				if cls.IngressRate != "" {
					cmdDelFilterIngress := []string{"chroot", "/host", "tc", "filter", "del", "dev", targetIface, "parent", "ffff:", "protocol", proto}
					outIngress, errIngress := exec.Command(cmdDelFilterIngress[0], cmdDelFilterIngress[1:]...).CombinedOutput()
					if errIngress == nil {
						filtersRemoved++
						log.Info("[SELECTIVE-CLEANUP] Removed ingress policing filter", "interface", targetIface, "strategy", desc)
					} else {
						log.Info("[SELECTIVE-CLEANUP] Ingress filter note", "interface", targetIface, "strategy", desc, "details", strings.TrimSpace(string(outIngress)))
					}
				}
			}

			// 3. Selectively Delete HTB Class
			if classHandle != "" {
				cmdDelClass := []string{"chroot", "/host", "tc", "class", "del", "dev", targetIface, "parent", fmt.Sprintf("%d:1", rootHandle), "classid", classHandle}
				outClass, errClass := exec.Command(cmdDelClass[0], cmdDelClass[1:]...).CombinedOutput()
				if errClass == nil {
					classesRemoved++
					log.Info("[SELECTIVE-CLEANUP] Successfully deleted target HTB class", "interface", targetIface, "classHandle", classHandle)
				} else {
					log.Info("[SELECTIVE-CLEANUP] Class removal result", "interface", targetIface, "classHandle", classHandle, "output", strings.TrimSpace(string(outClass)))
				}
			}
		}
	}

	return classesRemoved, filtersRemoved
}
