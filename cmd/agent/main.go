package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"

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

	// 4. HTTP /stats Handler (Direct Netlink Querying with Filtering)
	http.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		iface := r.URL.Query().Get("interface")
		if iface == "" {
			iface = "enp1s0"
		}

		// Parse optional target VLAN or ClassID filters
		var targetVlan int
		if vlanStr := r.URL.Query().Get("vlan"); vlanStr != "" {
			targetVlan, _ = strconv.Atoi(vlanStr)
		}
		targetClassID := r.URL.Query().Get("classId")

		log.Info("[API] GET /stats", "clientIP", r.RemoteAddr, "interface", iface, "targetVlan", targetVlan, "targetClassID", targetClassID)

		// Build classMap (mapping "1:100" -> "storage-vlan-100") from active CRDs
		classMap := make(map[string]string)
		ctx := context.Background()
		var list networkingv1alpha1.VlanTrafficControlList
		if err := k8sClient.List(ctx, &list); err == nil {
			for _, item := range list.Items {
				rootHandle := item.Spec.HtbRoot.HtbID
				if rootHandle <= 0 {
					rootHandle = 1
				}
				for _, cls := range item.Spec.HtbRoot.Classes {
					cHandle := cls.GetClassID(rootHandle)
					if cls.Name != "" {
						classMap[cHandle] = cls.Name
					}
				}
			}
		}

		// Query statistics via high-performance Netlink socket
		stats, errStats := executor.GetInterfaceStatsFiltered(iface, classMap, targetVlan, targetClassID)
		if errStats != nil {
			log.Error(errStats, "[API] Failed retrieving Netlink stats", "interface", iface)
			http.Error(w, fmt.Sprintf("failed retrieving stats: %v", errStats), http.StatusInternalServerError)
			return
		}

		log.Info("[API] GET /stats completed",
			"interface", iface,
			"classesFound", len(stats.ClassStats),
			"ingressRulesFound", len(stats.IngressStats),
		)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(stats)
	})

	// 5. HTTP /cleanup Handler (Full Root & Ingress Qdisc Wipe)
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
		log.Info("[CLEANUP] Starting host TC rule cleanup...", "interface", iface, "triggeredBy", r.RemoteAddr)

		// Deletes root HTB qdisc and ingress policing qdisc via FlushInterface
		errCleanup := executor.FlushInterface(iface)
		if errCleanup != nil {
			log.Error(errCleanup, "[CLEANUP] Error during interface flush", "interface", iface)
		} else {
			log.Info("[CLEANUP] Interface successfully flushed (root & ingress qdiscs removed)", "interface", iface)
		}
		log.Info("========================================================================")

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"status":    "cleanup_completed",
			"node":      nodeName,
			"interface": iface,
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

        // 7. HTTP /config Handler (Full or Partial Alignment Verification)
        http.HandleFunc("/config", func(w http.ResponseWriter, r *http.Request) {
            iface := r.URL.Query().Get("interface")
            if iface == "" {
                iface = "enp1s0"
            }

            targetClassID := r.URL.Query().Get("classId") // Optional partial query parameter

            // 1. Aggregate planned CRDs active for this node
            ctx := context.Background()
            var list networkingv1alpha1.VlanTrafficControlList
            if err := k8sClient.List(ctx, &list); err != nil {
                http.Error(w, "failed listing CRD specs", http.StatusInternalServerError)
                return
            }

            // Merge planned root specs for target interface
            var aggregatedSpec networkingv1alpha1.HtbRootSpec
            aggregatedSpec.Interface = iface
            aggregatedSpec.Rate = "10Gbit" // Default root capacity

            for _, item := range list.Items {
                if item.Spec.HtbRoot.Interface == iface {
                    aggregatedSpec.Classes = append(aggregatedSpec.Classes, item.Spec.HtbRoot.Classes...)
                }
            }

            // 2. Perform Netlink Drift Analysis
            report, err := executor.InspectNodeAlignment(&aggregatedSpec, targetClassID)
            if err != nil {
                http.Error(w, fmt.Sprintf("alignment check failed: %v", err), http.StatusInternalServerError)
                return
            }

            w.Header().Set("Content-Type", "application/json")
            w.WriteHeader(http.StatusOK)
            _ = json.NewEncoder(w).Encode(report)
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
