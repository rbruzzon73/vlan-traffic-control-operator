package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"go.uber.org/zap"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	networkingv1alpha1 "networking.med.io/vlan-traffic-control/api/v1alpha1"
	"networking.med.io/vlan-traffic-control/pkg/executor"
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
	reconcileLocalTc(k8sClient, nodeName, log)

	// 4. HTTP /stats Handler
	http.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		iface := r.URL.Query().Get("interface")
		if iface == "" {
			iface = "enp1s0"
		}

		var targetVlan int
		if vlanStr := r.URL.Query().Get("vlan"); vlanStr != "" {
			targetVlan, _ = strconv.Atoi(vlanStr)
		}
		targetClassID := r.URL.Query().Get("classId")

		log.Info("[API] GET /stats", "clientIP", r.RemoteAddr, "interface", iface, "targetVlan", targetVlan, "targetClassID", targetClassID)

		ctx := context.Background()
		hostNode := getHostNode(ctx, k8sClient, nodeName, log)

		classMap := make(map[string]string)
		var list networkingv1alpha1.VlanTrafficControlList
		if err := k8sClient.List(ctx, &list); err == nil {
			for _, item := range list.Items {
				if !isPolicyTargetingNode(hostNode, nodeName, &item, log) {
					continue
				}

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

	// 5. HTTP /cleanup Handler
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
		reconcileLocalTc(k8sClient, nodeName, log)

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"reconciled","node":"` + nodeName + `"}`))
	})

	// 7. HTTP /config Handler
	http.HandleFunc("/config", func(w http.ResponseWriter, r *http.Request) {
		iface := r.URL.Query().Get("interface")
		if iface == "" {
			iface = "enp1s0"
		}

		targetClassID := r.URL.Query().Get("classId")

		ctx := context.Background()
		hostNode := getHostNode(ctx, k8sClient, nodeName, log)

		var list networkingv1alpha1.VlanTrafficControlList
		if err := k8sClient.List(ctx, &list); err != nil {
			http.Error(w, "failed listing CRD specs", http.StatusInternalServerError)
			return
		}

		var aggregatedSpec networkingv1alpha1.HtbRootSpec
		aggregatedSpec.Interface = iface
		aggregatedSpec.Rate = "10Gbit"

		hasMatchingPolicy := false
		for _, item := range list.Items {
			if !isPolicyTargetingNode(hostNode, nodeName, &item, log) {
				continue
			}

			if item.Spec.HtbRoot.Interface == iface {
				hasMatchingPolicy = true
				aggregatedSpec.Classes = append(aggregatedSpec.Classes, item.Spec.HtbRoot.Classes...)
			}
		}

		if !hasMatchingPolicy {
			log.Info("[CONFIG] No active VlanTrafficControl policy targets interface on this node", "nodeName", nodeName, "interface", iface)
		}

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

func reconcileLocalTc(k8sClient client.Client, nodeName string, log logr.Logger) {
	ctx := context.Background()

	hostNode := getHostNode(ctx, k8sClient, nodeName, log)

	var list networkingv1alpha1.VlanTrafficControlList
	if err := k8sClient.List(ctx, &list); err != nil {
		log.Error(err, "[RECONCILE] Failed listing VlanTrafficControl CRDs")
		return
	}

	specsByInterface := make(map[string]*networkingv1alpha1.HtbRootSpec)
	strategyByInterface := make(map[string]networkingv1alpha1.TcStrategyType)

	allKnownInterfaces := make(map[string]bool)

	for _, item := range list.Items {
		iface := item.Spec.HtbRoot.Interface
		if iface == "" {
			continue
		}
		allKnownInterfaces[iface] = true

		if !isPolicyTargetingNode(hostNode, nodeName, &item, log) {
			log.Info("[RECONCILE] Skipping CRD instance (does not target host node or missing toleration)", "instance", item.Name, "nodeName", nodeName)
			continue
		}

		aggSpec, exists := specsByInterface[iface]
		if !exists {
			aggSpec = &networkingv1alpha1.HtbRootSpec{
				Interface:         iface,
				HtbID:             item.Spec.HtbRoot.HtbID,
				Rate:              item.Spec.HtbRoot.Rate,
				DefaultClassMinor: item.Spec.HtbRoot.DefaultClassMinor,
				Classes:           []networkingv1alpha1.ClassSpec{},
			}
			specsByInterface[iface] = aggSpec
			strategyByInterface[iface] = item.Spec.TcStrategy
		}

		aggSpec.Classes = append(aggSpec.Classes, item.Spec.HtbRoot.Classes...)
	}

	for iface := range allKnownInterfaces {
		if _, targeted := specsByInterface[iface]; !targeted {
			log.Info("🧹 [RECONCILE] No active policies target this node for interface. Flushing interface TC rules...", "interface", iface)
			if err := executor.FlushInterface(iface); err != nil {
				log.Error(err, "[RECONCILE] Failed flushing untargeted interface", "interface", iface)
			} else {
				log.Info("✅ [RECONCILE] Interface TC rules successfully flushed", "interface", iface)
			}
		}
	}

	for iface, aggSpec := range specsByInterface {
		strategy := strategyByInterface[iface]
		log.Info("----------------------------------------------------------------")
		log.Info("[RECONCILE] Applying Aggregated Interface Spec", "interface", iface, "totalClasses", len(aggSpec.Classes), "strategy", strategy)

		strategyUsed, err := executor.ApplyAdaptiveHtbHierarchy(aggSpec, strategy, log)
		if err != nil {
			log.Error(err, "[RECONCILE] Failed applying aggregated TC rules", "interface", iface, "strategy", strategyUsed)
		} else {
			log.Info("[RECONCILE] Successfully applied aggregated TC rules on host interface", "interface", iface, "strategy", strategyUsed)
		}
		log.Info("----------------------------------------------------------------")
	}
}

func getHostNode(ctx context.Context, k8sClient client.Client, nodeName string, log logr.Logger) *corev1.Node {
	var hostNode corev1.Node
	for attempts := 1; attempts <= 5; attempts++ {
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: nodeName}, &hostNode); err == nil && len(hostNode.Labels) > 0 {
			return &hostNode
		}
		time.Sleep(200 * time.Millisecond)
	}
	log.V(1).Info("[WARN] Could not fetch complete host node labels/taints from Kube API", "nodeName", nodeName)
	return &hostNode
}

func isPolicyTargetingNode(hostNode *corev1.Node, nodeName string, item *networkingv1alpha1.VlanTrafficControl, log logr.Logger) bool {
	if hostNode != nil && (len(hostNode.Labels) > 0 || len(hostNode.Spec.Taints) > 0) {
		return executor.IsPolicyTargetingNode(hostNode, item, log)
	}

	if len(item.Spec.NodeSelector) == 0 && item.Spec.NodeLabelSelector == nil {
		return true
	}

	isMasterName := strings.Contains(nodeName, "master") || strings.Contains(nodeName, "control-plane")

	if isMasterName && len(item.Spec.Tolerations) == 0 {
		log.Info("[NODE-FILTER-FALLBACK] Rejecting master node due to missing tolerations in fallback mode", "nodeName", nodeName, "crName", item.Name)
		return false
	}

	if item.Spec.NodeLabelSelector != nil && item.Spec.NodeLabelSelector.MatchLabels != nil {
		for reqKey, reqVal := range item.Spec.NodeLabelSelector.MatchLabels {
			if strings.HasPrefix(reqKey, "node-role.kubernetes.io/") {
				role := strings.TrimPrefix(reqKey, "node-role.kubernetes.io/")
				if role != "" && strings.Contains(nodeName, role) {
					return true
				}
			}
			if reqVal != "" && strings.Contains(nodeName, reqVal) {
				return true
			}
		}
	}

	for key, expectedVal := range item.Spec.NodeSelector {
		if strings.HasPrefix(key, "node-role.kubernetes.io/") {
			role := strings.TrimPrefix(key, "node-role.kubernetes.io/")
			if role != "" && strings.Contains(nodeName, role) {
				return true
			}
		}
		if expectedVal != "" && strings.Contains(nodeName, expectedVal) {
			return true
		}
	}

	return false
}
