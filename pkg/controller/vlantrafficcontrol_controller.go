package controller

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	networkingv1alpha1 "networking.med.io/vlan-traffic-control/api/v1alpha1"
	"networking.med.io/vlan-traffic-control/pkg/executor"
)

const (
	// Finalizer for external and dependent resource cleanup
	vlanTrafficControlFinalizer = "networking.med.io/finalizer"

	// Condition types and reasons
	TypeReady      = "Ready"
	TypeConfigured = "Configured"

	ReasonReconciling = "Reconciling"
	ReasonFailed      = "Failed"
	ReasonSuccessful  = "Successful"
	ReasonDeleting    = "Deleting"
)

type VlanTrafficControlReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=networking.med.io,resources=vlantrafficcontrols,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.med.io,resources=vlantrafficcontrols/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=networking.med.io,resources=vlantrafficcontrols/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=daemonsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods;services;nodes;configmaps;events,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=security.openshift.io,resources=securitycontextconstraints,resourceNames=privileged,verbs=use

func (r *VlanTrafficControlReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var instance networkingv1alpha1.VlanTrafficControl
	if err := r.Get(ctx, req.NamespacedName, &instance); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("VlanTrafficControl resource not found. Cleaning up dependent components.")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to fetch VlanTrafficControl resource")
		return ctrl.Result{}, err
	}

	// Resolve target namespace for the Agent DaemonSet
	targetNamespace := instance.Namespace
	if targetNamespace == "" {
		targetNamespace = req.Namespace
	}
	if targetNamespace == "" {
		targetNamespace = "openshift-vlan-tc-operator"
	}

	// 1. Handle Finalizers (Deletion Phase)
	isMarkedToBeDeleted := !instance.ObjectMeta.DeletionTimestamp.IsZero()
	if isMarkedToBeDeleted {
		if controllerutil.ContainsFinalizer(&instance, vlanTrafficControlFinalizer) {
			logger.Info("Performing finalizer cleanup for VlanTrafficControl", "name", instance.Name)

			// Update condition to signal deletion progress
			r.updateStatusCondition(&instance, TypeReady, metav1.ConditionFalse, ReasonDeleting, "Deleting TC rules on worker nodes")
			_ = r.Status().Update(ctx, &instance)

			// Execute TC rule cleanup across node agent pods
			if err := r.cleanupNodeTrafficControl(ctx, &instance, targetNamespace); err != nil {
				logger.Error(err, "Failed to clean up TC rules on worker nodes during CR deletion")
				return ctrl.Result{RequeueAfter: 5 * time.Second}, err
			}

			// Remove finalizer
			controllerutil.RemoveFinalizer(&instance, vlanTrafficControlFinalizer)
			if err := r.Update(ctx, &instance); err != nil {
				logger.Error(err, "Failed to remove finalizer")
				return ctrl.Result{}, err
			}
			logger.Info("Successfully finalized VlanTrafficControl and wiped node TC rules")
		}
		return ctrl.Result{}, nil
	}

	// 2. Ensure Finalizer is Present
	if !controllerutil.ContainsFinalizer(&instance, vlanTrafficControlFinalizer) {
		controllerutil.AddFinalizer(&instance, vlanTrafficControlFinalizer)
		if err := r.Update(ctx, &instance); err != nil {
			logger.Error(err, "Failed to add finalizer")
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// 3. Log Safely Resolved TC Class Handles
	rootHandle := resolveRootHandle(instance.Spec.HtbRoot.HtbID)
	defaultClassHandle := resolveDefaultClassHandle(rootHandle, instance.Spec.HtbRoot.DefaultClassMinor)
	
	logger.V(1).Info("Resolved HTB handles", 
		"interface", instance.Spec.HtbRoot.Interface, 
		"rootHandle", fmt.Sprintf("%d:", rootHandle), 
		"defaultClassHandle", defaultClassHandle,
	)

	for _, class := range instance.Spec.HtbRoot.Classes {
		classHandle := resolveClassHandle(rootHandle, class.ClassMinor)
		logger.V(1).Info("Mapped TC Class Handle", "classMinor", class.ClassMinor, "fullHandle", classHandle)
	}

	// 4. Calculate Spec SHA256 Hash to trigger Agent pod restarts on CRD spec changes
	specBytes, err := json.Marshal(instance.Spec)
	if err != nil {
		logger.Error(err, "Failed to marshal spec for hashing")
		r.updateStatusCondition(&instance, TypeReady, metav1.ConditionFalse, ReasonFailed, "Failed to marshal spec")
		_ = r.Status().Update(ctx, &instance)
		return ctrl.Result{}, err
	}
	specHash := fmt.Sprintf("%x", sha256.Sum256(specBytes))

	// 5. Build or Update the Agent DaemonSet Manifest
	agentDaemonSet := r.buildAgentDaemonSet(&instance, targetNamespace, specHash)

	// Avoid cross-namespace controller reference errors if instance is cluster-scoped
	if instance.Namespace != "" {
		if err := ctrl.SetControllerReference(&instance, agentDaemonSet, r.Scheme); err != nil {
			logger.Error(err, "Failed to set controller reference on DaemonSet")
			return ctrl.Result{}, err
		}
	}

	var existingDS appsv1.DaemonSet
	err = r.Get(ctx, client.ObjectKey{Name: agentDaemonSet.Name, Namespace: targetNamespace}, &existingDS)
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Info("Creating Agent DaemonSet across worker nodes", "namespace", targetNamespace)
			if err := r.Create(ctx, agentDaemonSet); err != nil {
				r.updateStatusCondition(&instance, TypeReady, metav1.ConditionFalse, ReasonFailed, fmt.Sprintf("Failed to create DaemonSet: %v", err))
				_ = r.Status().Update(ctx, &instance)
				return ctrl.Result{}, fmt.Errorf("failed to create Agent DaemonSet: %w", err)
			}
		} else {
			logger.Error(err, "Failed to fetch existing Agent DaemonSet")
			return ctrl.Result{}, err
		}
	} else {
		if existingDS.Spec.Template.Annotations == nil {
			existingDS.Spec.Template.Annotations = make(map[string]string)
		}

		// Trigger rolling update if the config hash or node selector has changed
		if existingDS.Spec.Template.Annotations["networking.med.io/config-hash"] != specHash {
			existingDS.Spec.Template.Annotations["networking.med.io/config-hash"] = specHash
			existingDS.Spec.Template.Spec.NodeSelector = instance.Spec.NodeSelector

			logger.Info("Updating Agent DaemonSet config hash - triggering rolling restart of agent pods")
			if err := r.Update(ctx, &existingDS); err != nil {
				r.updateStatusCondition(&instance, TypeReady, metav1.ConditionFalse, ReasonFailed, fmt.Sprintf("Failed to update DaemonSet: %v", err))
				_ = r.Status().Update(ctx, &instance)
				return ctrl.Result{}, fmt.Errorf("failed to update Agent DaemonSet: %w", err)
			}
		}
	}

	// 6. Aggregate Performance Metrics from Agent Pods
	r.collectAgentPerformanceStats(ctx, &instance, targetNamespace)

	// 7. Update Status Conditions to Ready
	instance.Status.ObservedGeneration = instance.Generation
	r.updateStatusCondition(&instance, TypeConfigured, metav1.ConditionTrue, ReasonSuccessful, "Agent DaemonSet active")
	r.updateStatusCondition(&instance, TypeReady, metav1.ConditionTrue, ReasonSuccessful, "Traffic control operator synchronized")

	if err := r.Status().Update(ctx, &instance); err != nil {
		logger.Error(err, "Failed to update status for VlanTrafficControl")
		return ctrl.Result{}, err
	}

	reconcileInterval := time.Duration(instance.Spec.ReconcileIntervalSeconds) * time.Second
	if reconcileInterval <= 0 {
		reconcileInterval = 60 * time.Second
	}

	return ctrl.Result{RequeueAfter: reconcileInterval}, nil
}

// Helper functions for safe TC handle resolution
func resolveRootHandle(htbID int) int {
	if htbID <= 0 {
		return 1
	}
	return htbID
}

func resolveDefaultClassHandle(rootHandle int, defaultMinor int) string {
	if defaultMinor <= 0 {
		defaultMinor = 99
	}
	return fmt.Sprintf("%d:%d", rootHandle, defaultMinor)
}

func resolveClassHandle(rootHandle int, classMinor int) string {
	return fmt.Sprintf("%d:%d", rootHandle, classMinor)
}

// cleanupNodeTrafficControl issues DELETE requests to agent pods to flush host qdiscs and filters
func (r *VlanTrafficControlReconciler) cleanupNodeTrafficControl(ctx context.Context, instance *networkingv1alpha1.VlanTrafficControl, namespace string) error {
	logger := log.FromContext(ctx)

	var podList corev1.PodList
	if err := r.List(ctx, &podList, client.InNamespace(namespace), client.MatchingLabels{"app": "vlan-traffic-control-agent"}); err != nil {
		return fmt.Errorf("failed listing agent pods for cleanup: %w", err)
	}

	httpClient := &http.Client{Timeout: 5 * time.Second}

	for _, agentPod := range podList.Items {
		if agentPod.Status.Phase != corev1.PodRunning || agentPod.Status.PodIP == "" {
			continue
		}

		url := fmt.Sprintf("http://%s:8080/cleanup?interface=%s", agentPod.Status.PodIP, instance.Spec.HtbRoot.Interface)
		req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
		if err != nil {
			logger.Error(err, "Failed to create cleanup request", "node", agentPod.Spec.NodeName)
			continue
		}

		resp, err := httpClient.Do(req)
		if err != nil {
			logger.Error(err, "Failed to trigger cleanup on agent pod", "node", agentPod.Spec.NodeName)
			continue
		}
		_ = resp.Body.Close()

		logger.Info("Successfully triggered TC cleanup on worker node", "node", agentPod.Spec.NodeName, "interface", instance.Spec.HtbRoot.Interface)
	}

	return nil
}

func (r *VlanTrafficControlReconciler) buildAgentDaemonSet(instance *networkingv1alpha1.VlanTrafficControl, namespace, specHash string) *appsv1.DaemonSet {
	privilegedVal := true
	hostPathDir := corev1.HostPathDirectory

	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "vlan-traffic-control-agent",
			Namespace: namespace,
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": "vlan-traffic-control-agent",
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": "vlan-traffic-control-agent",
					},
					Annotations: map[string]string{
						"networking.med.io/config-hash": specHash,
					},
				},
				Spec: corev1.PodSpec{
					HostNetwork:        true,
					HostPID:            true,
					ServiceAccountName: "vlan-traffic-control-manager",
					NodeSelector:       instance.Spec.NodeSelector,
					Containers: []corev1.Container{
						{
							Name:            "agent",
							Image:           "ghcr.io/rbruzzon73/vlan-traffic-control-agent:v0.2.4",
							ImagePullPolicy: corev1.PullAlways,
							SecurityContext: &corev1.SecurityContext{
								Privileged: &privilegedVal,
							},
							Env: []corev1.EnvVar{
								{
									Name: "NODE_NAME",
									ValueFrom: &corev1.EnvVarSource{
										FieldRef: &corev1.ObjectFieldSelector{
											FieldPath: "spec.nodeName",
										},
									},
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "host-root",
									MountPath: "/host",
									ReadOnly:  false,
								},
								{
									Name:      "host-modules",
									MountPath: "/lib/modules",
									ReadOnly:  true,
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "host-root",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/",
									Type: &hostPathDir,
								},
							},
						},
						{
							Name: "host-modules",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/lib/modules",
									Type: &hostPathDir,
								},
							},
						},
					},
				},
			},
		},
	}
}

func (r *VlanTrafficControlReconciler) collectAgentPerformanceStats(ctx context.Context, instance *networkingv1alpha1.VlanTrafficControl, namespace string) {
	logger := log.FromContext(ctx)

	var podList corev1.PodList
	if err := r.List(ctx, &podList, client.InNamespace(namespace), client.MatchingLabels{"app": "vlan-traffic-control-agent"}); err != nil {
		logger.Error(err, "Failed listing Agent Pods for metrics collection")
		return
	}

	httpClient := &http.Client{Timeout: 3 * time.Second}

	for _, agentPod := range podList.Items {
		if agentPod.Status.Phase != corev1.PodRunning || agentPod.Status.PodIP == "" {
			continue
		}

		url := fmt.Sprintf("http://%s:8080/stats?interface=%s", agentPod.Status.PodIP, instance.Spec.HtbRoot.Interface)
		resp, err := httpClient.Get(url)
		if err != nil {
			logger.V(1).Info("Could not poll agent stats", "node", agentPod.Spec.NodeName, "error", err)
			continue
		}

		var statsData struct {
			Node         string                        `json:"node"`
			ClassStats   []executor.ClassStats         `json:"classStats"`
			IngressStats []executor.IngressFilterStats `json:"ingressStats"`
		}

		if err := json.NewDecoder(resp.Body).Decode(&statsData); err == nil {
			logger.V(1).Info("Collected agent stats", "node", agentPod.Spec.NodeName, "classCount", len(statsData.ClassStats), "ingressCount", len(statsData.IngressStats))
		}
		_ = resp.Body.Close()
	}
}

func (r *VlanTrafficControlReconciler) updateStatusCondition(instance *networkingv1alpha1.VlanTrafficControl, conditionType string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&instance.Status.Conditions, metav1.Condition{
		Type:               conditionType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: instance.Generation,
		LastTransitionTime: metav1.Now(),
	})
}

func (r *VlanTrafficControlReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&networkingv1alpha1.VlanTrafficControl{}).
		Owns(&appsv1.DaemonSet{}).
		Complete(r)
}
