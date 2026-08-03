package executor

import (
	"fmt"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	networkingv1alpha1 "networking.med.io/vlan-traffic-control/api/v1alpha1"
)

// IsPolicyTargetingNode determines if a CR policy applies to the local host node.
// Supports both legacy spec.NodeSelector maps and rich spec.NodeLabelSelector expressions,
// while enforcing node taint/toleration compliance and dumping diagnostic logs.
func IsPolicyTargetingNode(node *corev1.Node, cr *networkingv1alpha1.VlanTrafficControl, log logr.Logger) bool {
	if node == nil {
		log.Info("[NODE-FILTER] Node object is nil - skipping CR evaluation", "cr", cr.Name)
		return false
	}

	nodeLabels := labels.Set(node.Labels)

	// 1. Check API-level NodeLabelSelector (matchLabels & matchExpressions)
	if cr.Spec.NodeLabelSelector != nil {
		selector, err := metav1.LabelSelectorAsSelector(cr.Spec.NodeLabelSelector)
		if err == nil {
			if !selector.Matches(nodeLabels) {
				log.V(1).Info("[NODE-FILTER] NodeLabelSelector did NOT match node",
					"node", node.Name,
					"cr", cr.Name,
					"selector", cr.Spec.NodeLabelSelector,
				)
				return false
			}
		}
	}

	// 2. Check legacy map-based NodeSelector
	if len(cr.Spec.NodeSelector) > 0 {
		for key, expectedVal := range cr.Spec.NodeSelector {
			actualVal, exists := node.Labels[key]
			if !exists {
				log.V(1).Info("[NODE-FILTER] NodeSelector label missing on node",
					"node", node.Name,
					"cr", cr.Name,
					"missingKey", key,
				)
				return false
			}
			if expectedVal != "" && actualVal != expectedVal {
				log.V(1).Info("[NODE-FILTER] NodeSelector label value mismatch",
					"node", node.Name,
					"cr", cr.Name,
					"key", key,
					"expected", expectedVal,
					"actual", actualVal,
				)
				return false
			}
		}
	}

	// Convert local TolerationSpec array to corev1.Toleration
	var coreTolerations []corev1.Toleration
	for _, t := range cr.Spec.Tolerations {
		coreTolerations = append(coreTolerations, t.ToCoreV1())
	}

	// Dump Taints vs Tolerations comparison in Agent Logs for diagnostics
	var nodeTaintsFormatted []string
	for _, t := range node.Spec.Taints {
		nodeTaintsFormatted = append(nodeTaintsFormatted, fmt.Sprintf("%s=%s:%s", t.Key, t.Value, t.Effect))
	}

	var crTolerationsFormatted []string
	for _, tol := range coreTolerations {
		crTolerationsFormatted = append(crTolerationsFormatted, fmt.Sprintf("key=%s, op=%s, val=%s, effect=%s", tol.Key, tol.Operator, tol.Value, tol.Effect))
	}

	// 3. Evaluate Taints against Tolerations
	for _, taint := range node.Spec.Taints {
		if taint.Effect == corev1.TaintEffectNoSchedule || taint.Effect == corev1.TaintEffectNoExecute {
			if !hasMatchingToleration(taint, coreTolerations) {
				log.Info("❌ [NODE-FILTER] CR rejected - Node taint NOT tolerated by CR",
					"nodeName", node.Name,
					"crName", cr.Name,
					"unhandledTaint", fmt.Sprintf("%s=%s:%s", taint.Key, taint.Value, taint.Effect),
					"allNodeTaints", nodeTaintsFormatted,
					"crTolerations", crTolerationsFormatted,
				)
				return false
			}
		}
	}

	log.V(1).Info("✅ [NODE-FILTER] CR successfully matched node labels and taints",
		"nodeName", node.Name,
		"crName", cr.Name,
		"nodeTaints", nodeTaintsFormatted,
		"crTolerations", crTolerationsFormatted,
	)

	return true
}

// hasMatchingToleration checks if a given node taint is tolerated by any of the CR's tolerations.
func hasMatchingToleration(taint corev1.Taint, tolerations []corev1.Toleration) bool {
	for _, tol := range tolerations {
		// Treat master and control-plane keys as equivalent on OpenShift
		keyMatches := (tol.Key == taint.Key) ||
			(tol.Key == "node-role.kubernetes.io/master" && taint.Key == "node-role.kubernetes.io/control-plane") ||
			(tol.Key == "node-role.kubernetes.io/control-plane" && taint.Key == "node-role.kubernetes.io/master")

		// 1. Key check: Empty key tolerates all keys; otherwise keys must match
		if tol.Key != "" && !keyMatches {
			continue
		}

		// 2. Effect check: Empty effect tolerates all effects; otherwise effects must match
		if tol.Effect != "" && tol.Effect != taint.Effect {
			continue
		}

		// 3. Operator check:
		// - "Exists" (or default/empty operator) matches any value
		// - "Equal" requires exact value equality
		if tol.Operator == corev1.TolerationOpExists || tol.Operator == "" {
			return true
		}
		if tol.Operator == corev1.TolerationOpEqual && tol.Value == taint.Value {
			return true
		}
	}
	return false
}
