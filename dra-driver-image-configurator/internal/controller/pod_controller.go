package controller

import (
	"context"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	imagev1alpha1 "github.com/gke-labs/dra-drivers/dra-driver-image-configurator/api/v1alpha1"
)

const BindingConditionUpdateImage = "image-configurator.x-k8s.io/image-updated"
const BindingFailureConditionUpdateImage = "image-configurator.x-k8s.io/image-update-failed"

// PodReconciler watches Pods nominated to a node and patches their
// container images based on the associated ResourceClaim config.
type PodReconciler struct {
	Client   client.Client
	Recorder events.EventRecorder
}

// SetupWithManager registers the controller with the manager.
func (r *PodReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Pod{}).
		Complete(r)
}

// claimBindingResult pairs a ResourceClaim with its pending binding results.
type claimBindingResult struct {
	Claim   *resourceapi.ResourceClaim
	Results []resourceapi.DeviceRequestAllocationResult
}

// Reconcile handles a single Pod event.
func (r *PodReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	var pod corev1.Pod
	if err := r.Client.Get(ctx, req.NamespacedName, &pod); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	if pod.DeletionTimestamp != nil {
		return reconcile.Result{}, nil
	}

	claims, err := r.fetchClaims(ctx, &pod)
	if err != nil {
		return reconcile.Result{}, err
	}

	bindingResults := collectPendingBindingResults(claims)
	if len(bindingResults) == 0 {
		return reconcile.Result{}, nil
	}

	imageConfigs, err := r.collectImageConfigs(claims)
	if err != nil {
		if r.Recorder != nil {
			r.Recorder.Eventf(&pod, nil, corev1.EventTypeWarning, "ImageConfigError", "CollectImageConfig", "Configuration error in ImageConfig: %v", err)
		}
		return reconcile.Result{}, err
	}
	if len(imageConfigs) == 0 {
		err := reconcile.TerminalError(fmt.Errorf("no valid ImageConfig found in claims matching the pod containers %s", req.NamespacedName))
		if r.Recorder != nil {
			r.Recorder.Eventf(&pod, nil, corev1.EventTypeWarning, "ImageConfigNotFound", "CollectImageConfig", "Configuration error in ImageConfig: %v", err)
		}
		return reconcile.Result{}, err
	}

	if err := r.patchImages(ctx, &pod, imageConfigs); err != nil {
		return reconcile.Result{}, err
	}

	for _, cbr := range bindingResults {
		if err := r.setBindingCondition(ctx, cbr); err != nil {
			return reconcile.Result{}, err
		}
	}

	return reconcile.Result{}, nil
}

// fetchClaims returns all ResourceClaims referenced by the pod.
func (r *PodReconciler) fetchClaims(ctx context.Context, pod *corev1.Pod) ([]*resourceapi.ResourceClaim, error) {
	var claimNames []string
	// Template-generated claims are surfaced by kubelet in Status.ResourceClaimStatuses.
	for _, rcs := range pod.Status.ResourceClaimStatuses {
		if rcs.ResourceClaimName == nil {
			continue
		}
		claimNames = append(claimNames, *rcs.ResourceClaimName)
	}
	// Static claims are referenced directly from Spec.ResourceClaims.
	for _, rc := range pod.Spec.ResourceClaims {
		if rc.ResourceClaimName == nil {
			continue
		}
		claimNames = append(claimNames, *rc.ResourceClaimName)
	}

	var claims []*resourceapi.ResourceClaim
	for _, name := range claimNames {
		claimKey := types.NamespacedName{
			Namespace: pod.Namespace,
			Name:      name,
		}
		claim := &resourceapi.ResourceClaim{}
		if err := r.Client.Get(ctx, claimKey, claim); err != nil {
			return nil, fmt.Errorf("get claim %s: %w", claimKey, err)
		}
		if claim.Status.Allocation == nil {
			return nil, fmt.Errorf("claim %s not yet allocated", claimKey)
		}
		claims = append(claims, claim)
	}
	return claims, nil
}

// collectPendingBindingResults returns per-claim pending binding results where the
// "image-configurator.x-k8s.io/image-updated" binding condition is required but not yet satisfied.
func collectPendingBindingResults(claims []*resourceapi.ResourceClaim) []claimBindingResult {
	var pending []claimBindingResult
	for _, claim := range claims {
		var results []resourceapi.DeviceRequestAllocationResult
		for _, result := range claim.Status.Allocation.Devices.Results {
			if result.Driver != imagev1alpha1.DriverName {
				continue
			}
			if !slices.Contains(result.BindingConditions, BindingConditionUpdateImage) {
				continue
			}
			if isBindingConditionAlreadySet(claim, &result, BindingConditionUpdateImage) {
				continue
			}
			results = append(results, result)
		}
		if len(results) > 0 {
			pending = append(pending, claimBindingResult{Claim: claim, Results: results})
		}
	}
	return pending
}

// collectImageConfigs extracts all ImageConfig objects from the allocated device
// configs across all claims. Configs whose Opaque.Driver targets a different
// driver are skipped so that a ResourceClaim may carry configs for multiple
// drivers.
func (r *PodReconciler) collectImageConfigs(claims []*resourceapi.ResourceClaim) ([]*imagev1alpha1.ImageConfig, error) {
	var imageConfigs []*imagev1alpha1.ImageConfig
	imageMap := make(map[string]string)
	for _, claim := range claims {
		for _, cfg := range claim.Status.Allocation.Devices.Config {
			// Skip configs with an empty Requests field: it is impossible to tell
			// whether such a config was intentionally authored by the user, and
			// there is no way to determine which device the config applies to.
			if len(cfg.Requests) == 0 {
				if r.Recorder != nil {
					r.Recorder.Eventf(claim, nil, corev1.EventTypeWarning, "EmptyDeviceConfigRequests",
						"CollectImageConfig", "Device config from %q has empty Requests; set spec.devices.config[].requests to target specific device requests", cfg.Source)
				}
				continue
			}
			ic, err := imagev1alpha1.DecodeAndValidateOpaque(cfg.Opaque)
			if err != nil {
				return nil, reconcile.TerminalError(fmt.Errorf("claim %s/%s: %w", claim.Namespace, claim.Name, err))
			}
			if ic == nil {
				continue
			}
			if image, conflict := imageMap[ic.ContainerName]; conflict && image != ic.Image {
				return nil, reconcile.TerminalError(fmt.Errorf("conflicting ImageConfigs for container %q: %q vs %q", ic.ContainerName, image, ic.Image))
			}
			imageMap[ic.ContainerName] = ic.Image
			imageConfigs = append(imageConfigs, ic)
		}
	}
	return imageConfigs, nil
}

// isBindingConditionAlreadySet checks whether the given condition is already
// set to True in Status.Devices for the device identified by the allocation result.
func isBindingConditionAlreadySet(claim *resourceapi.ResourceClaim, result *resourceapi.DeviceRequestAllocationResult, condition string) bool {
	for _, ds := range claim.Status.Devices {
		if ds.Driver != result.Driver || ds.Pool != result.Pool || ds.Device != result.Device {
			continue
		}
		for _, c := range ds.Conditions {
			if c.Type == condition && c.Status == metav1.ConditionTrue {
				return true
			}
		}
	}
	return false
}

// patchImages updates container images on the pod according to the provided ImageConfigs.
// It emits a Pod event describing whether images were patched or already verified.
func (r *PodReconciler) patchImages(ctx context.Context, pod *corev1.Pod, imageConfigs []*imagev1alpha1.ImageConfig) error {
	log := ctrl.LoggerFrom(ctx)
	needsUpdate := false
	for _, ic := range imageConfigs {
		matched := false
		// apply overwrites the image of any container whose name matches ic.ContainerName.
		// It updates `matched` when a container name matches, and `needsUpdate` only when
		// the image actually differs, so we skip a no-op Pod update.
		apply := func(containers []corev1.Container) {
			for i := range containers {
				if containers[i].Name != ic.ContainerName {
					continue
				}
				matched = true
				if containers[i].Image == ic.Image {
					continue
				}
				containers[i].Image = ic.Image
				needsUpdate = true
			}
		}
		// InitContainers
		apply(pod.Spec.InitContainers)
		// Containers
		apply(pod.Spec.Containers)

		if !matched {
			if r.Recorder != nil {
				r.Recorder.Eventf(pod, nil, corev1.EventTypeWarning, "ImageConfigContainerNotFound", "PatchImage", "containerName %s in ImageConfig doesn't match any container in pod", ic.ContainerName)
			}
			return reconcile.TerminalError(fmt.Errorf("containerName %s in ImageConfig doesn't match any container in pod %s/%s", ic.ContainerName, pod.Namespace, pod.Name))
		}
	}
	if !needsUpdate {
		log.Info("image verified")
		return nil
	}
	if err := r.Client.Update(ctx, pod); err != nil {
		return fmt.Errorf("update pod %s/%s: %w", pod.Namespace, pod.Name, err)
	}
	log.Info("image patched")
	if r.Recorder != nil {
		r.Recorder.Eventf(pod, nil, corev1.EventTypeNormal, "ImagePatched", "PatchImage", "Container image(s) updated based on ResourceClaim config")
	}
	return nil
}

// setBindingCondition sets the "image-configurator.x-k8s.io/image-updated" binding condition to True
// on the claim's Status.Devices for all pending binding results in a single update.
func (r *PodReconciler) setBindingCondition(ctx context.Context, cbr claimBindingResult) error {
	now := metav1.Now()
	for _, result := range cbr.Results {
		cbr.Claim.Status.Devices = append(cbr.Claim.Status.Devices, resourceapi.AllocatedDeviceStatus{
			Driver:  result.Driver,
			Pool:    result.Pool,
			Device:  result.Device,
			ShareID: (*string)(result.ShareID),
			Conditions: []metav1.Condition{{
				Type:               BindingConditionUpdateImage,
				Status:             metav1.ConditionTrue,
				LastTransitionTime: now,
				Reason:             "ImagePatched",
				Message:            "Container image has been updated",
			}},
		})
	}
	if err := r.Client.Status().Update(ctx, cbr.Claim); err != nil {
		return fmt.Errorf("update claim status %s/%s: %w", cbr.Claim.Namespace, cbr.Claim.Name, err)
	}
	return nil
}
