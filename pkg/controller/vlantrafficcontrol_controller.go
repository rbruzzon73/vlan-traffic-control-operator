package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "networking.med.io/vlan-traffic-control/api/v1alpha1"
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
// +kubebuilder:rbac:groups=networking.med.io,resources=vlantrafficcontrolsclasses;vlantrafficcontrolclasses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.med.io,resources=vlantrafficcontrolsclasses/status;vlantrafficcontrolclasses/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=daemonsets;deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods;services;nodes;configmaps;events,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=security.openshift.io,resources=securitycontextconstraints,resourceNames=privileged,verbs=use

func (r *VlanTrafficControlReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var instance v1alpha1.VlanTrafficControl
	if err := r.Get(ctx, req.NamespacedName, &instance); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("VlanTrafficControl resource not found. Cleaning up dependent components.")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to fetch VlanTrafficControl resource")
		return ctrl.Result{}, err
	}

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

			r.updateStatusWithRetry(ctx, req.NamespacedName, func(cr *v1alpha1.VlanTrafficControl) {
				r.updateStatusCondition(cr, TypeReady, metav1.ConditionFalse, ReasonDeleting, "Cleaning up TC rules on target node agents")
			})

			if err := r.cleanupNodeTrafficControl(ctx, &instance, targetNamespace); err != nil {
				logger.Error(err, "Failed to clean up TC rules on node agents during CR deletion")
				return ctrl.Result{RequeueAfter: 5 * time.Second}, err
			}

			controllerutil.RemoveFinalizer(&instance, vlanTrafficControlFinalizer)
			if err := r.Update(ctx, &instance); err != nil {
				logger.Error(err, "Failed to remove finalizer")
				return ctrl.Result{}, err
			}
			logger.Info("Successfully finalized VlanTrafficControl and updated node TC rules")

			var crList v1alpha1.VlanTrafficControlList
			if err := r.List(ctx, &crList); err == nil && len(crList.Items) <= 1 {
				logger.Info("No remaining VlanTrafficControl CRs found - removing Agent DaemonSet")
				var ds appsv1.DaemonSet
				if err := r.Get(ctx, client.ObjectKey{Name: "vlan-traffic-control-agent", Namespace: targetNamespace}, &ds); err == nil {
					_ = r.Delete(ctx, &ds)
				}
			}
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
	defaultClassHandle := resolveDefaultClassHandle(rootHandle, instance.Spec.HtbRoot.DefaultClassID, instance.Spec.HtbRoot.DefaultClassMinor)

	logger.V(1).Info("Resolved HTB handles",
		"interface", instance.Spec.HtbRoot.Interface,
		"rootHandle", fmt.Sprintf("%d:", rootHandle),
		"defaultClassHandle", defaultClassHandle,
	)

	for _, class := range instance.Spec.HtbRoot.Classes {
		classHandle := resolveClassHandle(rootHandle, class.ClassID, class.ClassMinor)
		logger.V(1).Info("Mapped TC Class Handle", "classID", class.ClassID, "classMinor", class.ClassMinor, "fullHandle", classHandle)
	}

	// 4. Build Aggregated Tolerations for Agent DaemonSet
	var aggregatedNodeSelector map[string]string = nil
	var aggregatedTolerations []corev1.Toleration

	var allCRs v1alpha1.VlanTrafficControlList
	if err := r.List(ctx, &allCRs); err == nil {
		for _, cr := range allCRs.Items {
			for _, tol := range cr.Spec.Tolerations {
				aggregatedTolerations = append(aggregatedTolerations, tol.ToCoreV1())
			}
		}
	}

	// 5. Build or Update the Agent DaemonSet Manifest
	agentDaemonSet := r.buildAgentDaemonSet(&instance, targetNamespace, aggregatedNodeSelector, aggregatedTolerations)
	_ = r.setOperatorDeploymentOwnerRef(ctx, agentDaemonSet, targetNamespace)

	if instance.Namespace != "" {
		if err := ctrl.SetControllerReference(&instance, agentDaemonSet, r.Scheme); err != nil {
			logger.Error(err, "Failed to set controller reference on DaemonSet")
			return ctrl.Result{}, err
		}
	}

	var existingDS appsv1.DaemonSet
	err := r.Get(ctx, client.ObjectKey{Name: agentDaemonSet.Name, Namespace: targetNamespace}, &existingDS)
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Info("Creating Agent DaemonSet across cluster nodes", "namespace", targetNamespace)
			if err := r.Create(ctx, agentDaemonSet); err != nil {
				r.updateStatusWithRetry(ctx, req.NamespacedName, func(cr *v1alpha1.VlanTrafficControl) {
					r.updateStatusCondition(cr, TypeReady, metav1.ConditionFalse, ReasonFailed, fmt.Sprintf("Failed to create DaemonSet: %v", err))
				})
				return ctrl.Result{}, fmt.Errorf("failed to create Agent DaemonSet: %w", err)
			}
		} else {
			logger.Error(err, "Failed to fetch existing Agent DaemonSet")
			return ctrl.Result{}, err
		}
	} else {
		currentImage := ""
		if len(existingDS.Spec.Template.Spec.Containers) > 0 {
			currentImage = existingDS.Spec.Template.Spec.Containers[0].Image
		}
		desiredImage := getAgentImage()

		nodeSelectorChanged := !reflect.DeepEqual(existingDS.Spec.Template.Spec.NodeSelector, agentDaemonSet.Spec.Template.Spec.NodeSelector)
		tolerationsChanged := !reflect.DeepEqual(existingDS.Spec.Template.Spec.Tolerations, agentDaemonSet.Spec.Template.Spec.Tolerations)
		imageChanged := currentImage != desiredImage

		if nodeSelectorChanged || tolerationsChanged || imageChanged {
			logger.Info("Updating Agent DaemonSet - structural changes detected",
				"nodeSelectorChanged", nodeSelectorChanged,
				"tolerationsChanged", tolerationsChanged,
				"imageChanged", imageChanged,
				"currentImage", currentImage,
				"desiredImage", desiredImage,
			)

			existingDS.OwnerReferences = agentDaemonSet.OwnerReferences
			existingDS.Spec.Template.Spec.NodeSelector = agentDaemonSet.Spec.Template.Spec.NodeSelector
			existingDS.Spec.Template.Spec.Tolerations = agentDaemonSet.Spec.Template.Spec.Tolerations
			if len(existingDS.Spec.Template.Spec.Containers) > 0 {
				existingDS.Spec.Template.Spec.Containers[0].Image = desiredImage
			}

			if err := r.Update(ctx, &existingDS); err != nil {
				r.updateStatusWithRetry(ctx, req.NamespacedName, func(cr *v1alpha1.VlanTrafficControl) {
					r.updateStatusCondition(cr, TypeReady, metav1.ConditionFalse, ReasonFailed, fmt.Sprintf("Failed to update DaemonSet: %v", err))
				})
				return ctrl.Result{}, fmt.Errorf("failed to update Agent DaemonSet: %w", err)
			}
		}
	}

	// 6. Project individual HTB class items into VlanTrafficControlClass CRs (Deduplicated)
	if err := r.reconcileClassProjections(ctx, &instance); err != nil {
		logger.Error(err, "Failed reconciling VlanTrafficControlClass projections")
		return ctrl.Result{}, err
	}

	// 7. Notify agent pods to re-sync TC state
	r.triggerAgentReconcile(ctx, targetNamespace)

	// 8. Aggregate Performance Metrics from Agent Pods
	r.collectAgentPerformanceStats(ctx, &instance, targetNamespace)

	// 9. Update Status Conditions to Ready
	errStatus := r.updateStatusWithRetry(ctx, req.NamespacedName, func(cr *v1alpha1.VlanTrafficControl) {
		cr.Status.ObservedGeneration = cr.Generation
		r.updateStatusCondition(cr, TypeConfigured, metav1.ConditionTrue, ReasonSuccessful, "Agent DaemonSet active")
		r.updateStatusCondition(cr, TypeReady, metav1.ConditionTrue, ReasonSuccessful, "Traffic control operator synchronized")
	})
	if errStatus != nil {
		logger.Error(errStatus, "Failed to update status for VlanTrafficControl after retries")
		return ctrl.Result{}, errStatus
	}

	reconcileInterval := time.Duration(instance.Spec.ReconcileIntervalSeconds) * time.Second
	if reconcileInterval <= 0 {
		reconcileInterval = 60 * time.Second
	}

	return ctrl.Result{RequeueAfter: reconcileInterval}, nil
}

// reconcileClassProjections keeps secondary VlanTrafficControlClass projection resources updated, deduplicating classes per CR
func (r *VlanTrafficControlReconciler) reconcileClassProjections(ctx context.Context, vtc *v1alpha1.VlanTrafficControl) error {
	seenClassIDs := make(map[string]bool)

	for _, cls := range vtc.Spec.HtbRoot.Classes {
		classID := cls.GetClassID(vtc.Spec.HtbRoot.HtbID)
		if seenClassIDs[classID] {
			continue
		}
		seenClassIDs[classID] = true

		projName := fmt.Sprintf("%s-%s", vtc.Name, cls.Name)

		// Determine direction metadata dynamically
		direction := "ingress+egress"
		if cls.IngressRate != "" && cls.EgressRate == "" {
			direction = "ingress"
		} else if cls.EgressRate != "" && cls.IngressRate == "" {
			direction = "egress"
		}

		ingressCeilVal := cls.IngressCeil
		if ingressCeilVal == "" && cls.IngressRate != "" {
			ingressCeilVal = vtc.Spec.HtbRoot.Rate
		}

		proj := &v1alpha1.VlanTrafficControlClass{
			ObjectMeta: metav1.ObjectMeta{
				Name:      projName,
				Namespace: vtc.Namespace,
				Labels: map[string]string{
					"app.kubernetes.io/managed-by": "vlan-traffic-control-operator",
					"vlantrafficcontrol.parent":    vtc.Name,
				},
			},
		}

		_, err := controllerutil.CreateOrUpdate(ctx, r.Client, proj, func() error {
			proj.Spec = v1alpha1.VlanTrafficControlClassSpec{
				ClassName:     cls.Name,
				Direction:     direction,
				ClassID:       classID,
				MatchType:     cls.MatchType,
				VlanID:        cls.VlanID,
				Subnet:        cls.Subnet,
				Mark:          cls.Mark,
				IP:            cls.IP,
				Port:          cls.Port,
				Dscp:          cls.Dscp,
				Guaranteed:    cls.EgressRate,
				CeilBorrow:    cls.EgressCeil,
				EgressBurst:   cls.EgressBurst,
				EnableFqCodel: cls.EnableFqCodel,
				IngressRate:   cls.IngressRate,
				IngressCeil:   ingressCeilVal,
				IngressBurst:  cls.IngressBurst,
				IngressAction: cls.IngressAction,
				Priority:      cls.Priority,
				Aligned:       "True",
			}

			if err := controllerutil.SetControllerReference(vtc, proj, r.Scheme); err != nil {
				proj.OwnerReferences = []metav1.OwnerReference{
					*metav1.NewControllerRef(vtc, v1alpha1.GroupVersion.WithKind("VlanTrafficControl")),
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *VlanTrafficControlReconciler) updateStatusWithRetry(ctx context.Context, namespacedName types.NamespacedName, updateFn func(cr *v1alpha1.VlanTrafficControl)) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latestCR := &v1alpha1.VlanTrafficControl{}
		if err := r.Get(ctx, namespacedName, latestCR); err != nil {
			return err
		}
		updateFn(latestCR)
		return r.Status().Update(ctx, latestCR)
	})
}

func (r *VlanTrafficControlReconciler) setOperatorDeploymentOwnerRef(ctx context.Context, ds *appsv1.DaemonSet, namespace string) error {
	var depList appsv1.DeploymentList
	err := r.List(ctx, &depList, client.InNamespace(namespace), client.MatchingLabels{"control-plane": "controller-manager"})
	if err == nil && len(depList.Items) > 0 {
		operatorDep := &depList.Items[0]
		isController := false

		for _, owner := range ds.OwnerReferences {
			if owner.UID == operatorDep.UID {
				return nil
			}
		}

		ds.OwnerReferences = append(ds.OwnerReferences, metav1.OwnerReference{
			APIVersion:         "apps/v1",
			Kind:               "Deployment",
			Name:               operatorDep.Name,
			UID:                operatorDep.UID,
			BlockOwnerDeletion: nil,
			Controller:         &isController,
		})
	}
	return nil
}

func resolveRootHandle(htbID int) int {
	if htbID <= 0 {
		return 1
	}
	return htbID
}

func resolveDefaultClassHandle(rootHandle int, defaultClassID string, defaultMinor int) string {
	if defaultClassID != "" {
		return defaultClassID
	}
	if defaultMinor <= 0 {
		defaultMinor = 99
	}
	return fmt.Sprintf("%d:%d", rootHandle, defaultMinor)
}

func resolveClassHandle(rootHandle int, classID string, classMinor int) string {
	if classID != "" {
		return classID
	}
	return fmt.Sprintf("%d:%d", rootHandle, classMinor)
}

func (r *VlanTrafficControlReconciler) cleanupNodeTrafficControl(ctx context.Context, instance *v1alpha1.VlanTrafficControl, namespace string) error {
	logger := log.FromContext(ctx)

	var allCRs v1alpha1.VlanTrafficControlList
	if err := r.List(ctx, &allCRs); err != nil {
		return fmt.Errorf("failed listing CRs during cleanup evaluation: %w", err)
	}

	var podList corev1.PodList
	if err := r.List(ctx, &podList, client.InNamespace(namespace), client.MatchingLabels{"app": "vlan-traffic-control-agent"}); err != nil {
		return fmt.Errorf("failed listing agent pods for cleanup: %w", err)
	}

	httpClient := &http.Client{Timeout: 5 * time.Second}

	for _, agentPod := range podList.Items {
		if agentPod.Status.Phase != corev1.PodRunning || agentPod.Status.PodIP == "" {
			continue
		}

		var hostNode corev1.Node
		_ = r.Get(ctx, types.NamespacedName{Name: agentPod.Spec.NodeName}, &hostNode)

		nodeHasRemainingCR := false
		for _, cr := range allCRs.Items {
			if cr.Name == instance.Name {
				continue
			}
			if cr.Spec.HtbRoot.Interface == instance.Spec.HtbRoot.Interface {
				if executor.IsPolicyTargetingNode(&hostNode, &cr, logger) {
					nodeHasRemainingCR = true
					break
				}
			}
		}

		if nodeHasRemainingCR {
			url := fmt.Sprintf("http://%s:8080/reconcile", agentPod.Status.PodIP)
			resp, err := httpClient.Post(url, "application/json", nil)
			if err == nil {
				_ = resp.Body.Close()
			}
			logger.Info("Triggered selective re-sync on agent pod", "node", agentPod.Spec.NodeName, "interface", instance.Spec.HtbRoot.Interface)
		} else {
			url := fmt.Sprintf("http://%s:8080/cleanup?interface=%s", agentPod.Status.PodIP, instance.Spec.HtbRoot.Interface)
			req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
			if err != nil {
				continue
			}
			resp, err := httpClient.Do(req)
			if err == nil {
				_ = resp.Body.Close()
			}
			logger.Info("Flushed interface on node (no remaining CRs target this node)", "node", agentPod.Spec.NodeName, "interface", instance.Spec.HtbRoot.Interface)
		}
	}

	return nil
}

func getAgentImage() string {
	if img := os.Getenv("RELATED_IMAGE_AGENT"); img != "" {
		return img
	}
	return "ghcr.io/rbruzzon73/vlan-traffic-control-agent:v0.3.57"
}

func (r *VlanTrafficControlReconciler) buildAgentDaemonSet(
	instance *v1alpha1.VlanTrafficControl,
	namespace string,
	nodeSelector map[string]string,
	userTolerations []corev1.Toleration,
) *appsv1.DaemonSet {
	privilegedVal := true
	hostPathDir := corev1.HostPathDirectory

	baseTolerations := []corev1.Toleration{
		{
			Key:      "node-role.kubernetes.io/master",
			Operator: corev1.TolerationOpExists,
			Effect:   corev1.TaintEffectNoSchedule,
		},
		{
			Key:      "node-role.kubernetes.io/control-plane",
			Operator: corev1.TolerationOpExists,
			Effect:   corev1.TaintEffectNoSchedule,
		},
	}

	for _, userTol := range userTolerations {
		isDuplicate := false
		for _, baseTol := range baseTolerations {
			if userTol.Key == baseTol.Key && userTol.Effect == baseTol.Effect {
				isDuplicate = true
				break
			}
		}
		if !isDuplicate {
			baseTolerations = append(baseTolerations, userTol)
		}
	}

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
				},
				Spec: corev1.PodSpec{
					HostNetwork:        true,
					HostPID:            true,
					ServiceAccountName: "vlan-traffic-control-manager",
					NodeSelector:       nodeSelector,
					Tolerations:        baseTolerations,
					Containers: []corev1.Container{
						{
							Name:            "agent",
							Image:           getAgentImage(),
							ImagePullPolicy: corev1.PullAlways,
							Command:         []string{"/agent"},
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

func (r *VlanTrafficControlReconciler) collectAgentPerformanceStats(ctx context.Context, instance *v1alpha1.VlanTrafficControl, namespace string) {
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
			Node         string                 `json:"node"`
			ClassStats   []v1alpha1.ClassStat   `json:"classStats"`
			IngressStats []v1alpha1.IngressStat `json:"ingressStats"`
		}

		if err := json.NewDecoder(resp.Body).Decode(&statsData); err == nil {
			logger.V(1).Info("Collected agent stats", "node", agentPod.Spec.NodeName, "classCount", len(statsData.ClassStats), "ingressCount", len(statsData.IngressStats))
		}
		_ = resp.Body.Close()
	}
}

func (r *VlanTrafficControlReconciler) updateStatusCondition(instance *v1alpha1.VlanTrafficControl, conditionType string, status metav1.ConditionStatus, reason, message string) {
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
		For(&v1alpha1.VlanTrafficControl{}).
		Owns(&appsv1.DaemonSet{}).
		Owns(&v1alpha1.VlanTrafficControlClass{}).
		Complete(r)
}

func (r *VlanTrafficControlReconciler) triggerAgentReconcile(ctx context.Context, namespace string) {
	logger := log.FromContext(ctx)

	var podList corev1.PodList
	if err := r.List(ctx, &podList, client.InNamespace(namespace), client.MatchingLabels{"app": "vlan-traffic-control-agent"}); err != nil {
		logger.Error(err, "Failed listing agent pods for reconciliation trigger")
		return
	}

	httpClient := &http.Client{Timeout: 3 * time.Second}

	for _, agentPod := range podList.Items {
		if agentPod.Status.Phase != corev1.PodRunning || agentPod.Status.PodIP == "" {
			continue
		}

		url := fmt.Sprintf("http://%s:8080/reconcile", agentPod.Status.PodIP)
		resp, err := httpClient.Post(url, "application/json", nil)
		if err != nil {
			logger.V(1).Info("Could not send /reconcile trigger to agent pod", "node", agentPod.Spec.NodeName, "error", err)
			continue
		}
		_ = resp.Body.Close()
		logger.Info("Successfully triggered agent reconciliation", "node", agentPod.Spec.NodeName)
	}
}
