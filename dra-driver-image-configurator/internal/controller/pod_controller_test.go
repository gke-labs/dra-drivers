package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	imagev1alpha1 "github.com/gke-labs/dra-drivers/dra-driver-image-configurator/api/v1alpha1"
)

func createTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = resourceapi.AddToScheme(s)
	_ = imagev1alpha1.AddToScheme(s)
	return s
}

// ── builders ──────────────────────────────────────────────────────────────────

type claimOption func(*resourceapi.ResourceClaim)
type podOption func(*corev1.Pod)

// DeviceRef uniquely identifies a device and optionally declares binding conditions.
// Used as a named-argument style parameter across multiple builder functions.
type DeviceRef struct {
	Request           string
	Driver            string
	Pool              string
	Device            string
	ShareID           *types.UID
	BindingConditions []string
}

// ImageRef specifies a container image override delivered via ImageConfig.
// Source and Driver are used only when the ref is passed to withImageConfig
// (they are ignored by withContainer).
type ImageRef struct {
	Source        string
	Driver        string
	ContainerName string
	Image         string
	Requests      []string
}

// newImageConfig constructs an ImageConfig with the standard TypeMeta populated
// from ref.ContainerName / ref.Image. It is shared by withImageConfig and by tests
// that need ImageConfig values directly (e.g. TestPatchImages).
func newImageConfig(ref ImageRef) *imagev1alpha1.ImageConfig {
	return &imagev1alpha1.ImageConfig{
		TypeMeta: metav1.TypeMeta{
			APIVersion: imagev1alpha1.SchemeGroupVersion.String(),
			Kind:       "ImageConfig",
		},
		ContainerName: ref.ContainerName,
		Image:         ref.Image,
	}
}

// NameRef identifies a namespaced resource by name and namespace.
type NameRef struct {
	Name      string
	Namespace string
}

// newClaim builds a ResourceClaim with the given name/namespace.
func newClaim(ref NameRef, opts ...claimOption) *resourceapi.ResourceClaim {
	c := &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: ref.Name, Namespace: ref.Namespace},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// withResult appends a device allocation result to the claim.
func withResult(ref DeviceRef) claimOption {
	return func(c *resourceapi.ResourceClaim) {
		if c.Status.Allocation == nil {
			c.Status.Allocation = &resourceapi.AllocationResult{}
		}
		c.Status.Allocation.Devices.Results = append(
			c.Status.Allocation.Devices.Results,
			resourceapi.DeviceRequestAllocationResult{
				Request:           ref.Request,
				Driver:            ref.Driver,
				Pool:              ref.Pool,
				Device:            ref.Device,
				ShareID:           ref.ShareID,
				BindingConditions: ref.BindingConditions,
			},
		)
	}
}

// withDeviceCondition appends an AllocatedDeviceStatus with the given condition.
func withDeviceCondition(ref DeviceRef, condition string, status metav1.ConditionStatus) claimOption {
	return func(c *resourceapi.ResourceClaim) {
		c.Status.Devices = append(c.Status.Devices, resourceapi.AllocatedDeviceStatus{
			Driver: ref.Driver,
			Pool:   ref.Pool,
			Device: ref.Device,
			Conditions: []metav1.Condition{
				{Type: condition, Status: status},
			},
		})
	}
}

// withImageConfig appends an opaque DeviceAllocationConfiguration whose payload
// is an ImageConfig built from ref.ContainerName / ref.Image (JSON-marshaled).
func withImageConfig(t *testing.T, ref ImageRef) claimOption {
	t.Helper()
	raw, err := json.Marshal(newImageConfig(ref))
	if err != nil {
		t.Fatalf("marshal ImageConfig: %v", err)
	}
	return func(c *resourceapi.ResourceClaim) {
		if c.Status.Allocation == nil {
			c.Status.Allocation = &resourceapi.AllocationResult{}
		}
		c.Status.Allocation.Devices.Config = append(
			c.Status.Allocation.Devices.Config,
			resourceapi.DeviceAllocationConfiguration{
				Source:   resourceapi.AllocationConfigSource(ref.Source),
				Requests: ref.Requests,
				DeviceConfiguration: resourceapi.DeviceConfiguration{
					Opaque: &resourceapi.OpaqueDeviceConfiguration{
						Driver:     ref.Driver,
						Parameters: runtime.RawExtension{Raw: raw},
					},
				},
			},
		)
	}
}

// newPod builds a Pod with the given name/namespace.
func newPod(ref NameRef, opts ...podOption) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: ref.Name, Namespace: ref.Namespace},
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

// withContainer adds a container with the given name and image to the Pod.
func withContainer(ref ImageRef) podOption {
	return func(p *corev1.Pod) {
		p.Spec.Containers = append(p.Spec.Containers, corev1.Container{
			Name:  ref.ContainerName,
			Image: ref.Image,
		})
	}
}

// withClaimRef adds a PodResourceClaimStatus referencing the given claim.
func withClaimRef(claimName string) podOption {
	name := claimName
	return func(p *corev1.Pod) {
		p.Status.ResourceClaimStatuses = append(
			p.Status.ResourceClaimStatuses,
			corev1.PodResourceClaimStatus{Name: "ref", ResourceClaimName: &name},
		)
	}
}

// ── isBindingConditionAlreadySet ─────────────────────────────────────────────

func TestIsBindingConditionAlreadySet(t *testing.T) {
	tests := []struct {
		name      string
		claim     *resourceapi.ResourceClaim
		result    *resourceapi.DeviceRequestAllocationResult
		condition string
		want      bool
	}{
		{
			name: "expected condition to be already set",
			claim: newClaim(NameRef{Name: "c", Namespace: "default"},
				withDeviceCondition(
					DeviceRef{Driver: DriverName, Pool: "test-pool", Device: "test-device"},
					BindingConditionUpdateImage, metav1.ConditionTrue,
				),
			),
			result: &resourceapi.DeviceRequestAllocationResult{
				Driver: DriverName, Pool: "test-pool", Device: "test-device",
			},
			condition: BindingConditionUpdateImage,
			want:      true,
		},
		{
			name: "Test non-matching condition status",
			claim: newClaim(NameRef{Name: "c", Namespace: "default"},
				withDeviceCondition(
					DeviceRef{Driver: DriverName, Pool: "test-pool", Device: "test-device"},
					BindingConditionUpdateImage, metav1.ConditionFalse,
				),
			),
			result: &resourceapi.DeviceRequestAllocationResult{
				Driver: DriverName, Pool: "test-pool", Device: "test-device",
			},
			condition: BindingConditionUpdateImage,
			want:      false,
		},
		{
			name: "Test non-matching device",
			claim: newClaim(NameRef{Name: "c", Namespace: "default"},
				withDeviceCondition(
					DeviceRef{Driver: DriverName, Pool: "test-pool", Device: "test-device"},
					BindingConditionUpdateImage, metav1.ConditionTrue,
				),
			),
			result: &resourceapi.DeviceRequestAllocationResult{
				Driver: DriverName, Pool: "test-pool", Device: "other-device",
			},
			condition: BindingConditionUpdateImage,
			want:      false,
		},
		{
			name:  "Test empty devices list",
			claim: &resourceapi.ResourceClaim{},
			result: &resourceapi.DeviceRequestAllocationResult{
				Driver: DriverName, Pool: "test-pool", Device: "test-device",
			},
			condition: BindingConditionUpdateImage,
			want:      false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isBindingConditionAlreadySet(tc.claim, tc.result, tc.condition); got != tc.want {
				t.Errorf("isBindingConditionAlreadySet = %v, want %v", got, tc.want)
			}
		})
	}
}

// ── collectPendingBindingResults ──────────────────────────────────────────────

func TestCollectPendingBindingResults(t *testing.T) {
	shareID := types.UID("share-123")

	tests := []struct {
		name          string
		claims        []*resourceapi.ResourceClaim
		wantLen       int
		wantClaimName string
		wantDevice    string
		wantShareID   *types.UID
	}{
		{
			name: "returns only claims with pending binding condition not yet satisfied",
			claims: []*resourceapi.ResourceClaim{
				// claim-1: one device requires the binding condition (pending), another has no condition.
				newClaim(NameRef{Name: "claim-1", Namespace: "default"},
					withResult(DeviceRef{
						Request: "req-1", Driver: DriverName, Pool: "test-pool", Device: "dev-1",
						ShareID:           &shareID,
						BindingConditions: []string{BindingConditionUpdateImage},
					}),
					withResult(DeviceRef{
						Request: "req-other", Driver: "test-driver", Pool: "test-pool", Device: "dev-other",
					}),
				),
				// claim-2: condition is required but already set to True.
				newClaim(NameRef{Name: "claim-2", Namespace: "default"},
					withResult(DeviceRef{
						Request: "req-2", Driver: DriverName, Pool: "test-pool", Device: "dev-2",
						BindingConditions: []string{BindingConditionUpdateImage},
					}),
					withDeviceCondition(
						DeviceRef{Driver: DriverName, Pool: "test-pool", Device: "dev-2"},
						BindingConditionUpdateImage, metav1.ConditionTrue,
					),
				),
			},
			wantLen:       1,
			wantClaimName: "claim-1",
			wantDevice:    "dev-1",
			wantShareID:   &shareID,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pending := collectPendingBindingResults(tc.claims)

			if len(pending) != tc.wantLen {
				t.Fatalf("expected %d pending claim result, got %d", tc.wantLen, len(pending))
			}
			if pending[0].Claim.Name != tc.wantClaimName {
				t.Errorf("expected pending claim to be %s, got %s", tc.wantClaimName, pending[0].Claim.Name)
			}
			if len(pending[0].Results) != 1 {
				t.Fatalf("expected 1 pending device result, got %d", len(pending[0].Results))
			}
			if pending[0].Results[0].Device != tc.wantDevice {
				t.Errorf("expected pending device to be %s, got %s", tc.wantDevice, pending[0].Results[0].Device)
			}
			if pending[0].Results[0].ShareID == nil || *pending[0].Results[0].ShareID != *tc.wantShareID {
				t.Errorf("expected share ID to be %q, got %v", *tc.wantShareID, pending[0].Results[0].ShareID)
			}
		})
	}
}

// ── collectImageConfigs ───────────────────────────────────────────────────────

func TestCollectImageConfigs(t *testing.T) {
	tests := []struct {
		name              string
		claims            []*resourceapi.ResourceClaim
		wantLen           int
		wantContainerName string
		wantImage         string
		wantRequeue       bool
		errMsg            string
	}{
		{
			name: "decodes valid ImageConfig and skips missing entries",
			claims: []*resourceapi.ResourceClaim{
				newClaim(NameRef{Name: "c", Namespace: "default"},
					withResult(DeviceRef{Request: "req-1", Driver: "test-driver", Pool: "test-pool", Device: "dev-1"}),
					withResult(DeviceRef{Request: "req-2", Driver: "test-driver", Pool: "test-pool", Device: "dev-2"}),
					withImageConfig(t, ImageRef{
						Source:        "test-source",
						Driver:        DriverName,
						ContainerName: "test-container",
						Image:         "custom-image:v1",
						Requests:      []string{"req-1"},
					}),
					// Missing opaque.
					func(c *resourceapi.ResourceClaim) {
						c.Status.Allocation.Devices.Config = append(
							c.Status.Allocation.Devices.Config,
							resourceapi.DeviceAllocationConfiguration{Source: "other-source", Requests: []string{"req-2"}},
						)
					},
				),
			},
			wantLen:           1,
			wantContainerName: "test-container",
			wantImage:         "custom-image:v1",
		},
		{
			name: "returns TerminalError in case of opaque parameter decode failure",
			claims: []*resourceapi.ResourceClaim{
				newClaim(NameRef{Name: "c", Namespace: "default"},
					withResult(DeviceRef{Request: "req-1", Driver: "test-driver", Pool: "test-pool", Device: "dev-1"}),
					withResult(DeviceRef{Request: "req-2", Driver: "test-driver", Pool: "test-pool", Device: "dev-2"}),
					withImageConfig(t, ImageRef{
						Source:        "test-source",
						Driver:        DriverName,
						ContainerName: "test-container",
						Image:         "custom-image:v1",
						Requests:      []string{"req-1"},
					}),
					func(c *resourceapi.ResourceClaim) {
						c.Status.Allocation.Devices.Config = append(
							c.Status.Allocation.Devices.Config,
							resourceapi.DeviceAllocationConfiguration{
								Requests: []string{"req-2"},
								DeviceConfiguration: resourceapi.DeviceConfiguration{
									Opaque: &resourceapi.OpaqueDeviceConfiguration{
										Driver:     DriverName,
										Parameters: runtime.RawExtension{Raw: []byte("not-valid-json")},
									},
								},
							},
						)
					},
				),
			},
			wantLen:     0,
			wantRequeue: false,
			errMsg:      "Opaque parameter decode failure:",
		},
		{
			name: "skips opaque config targeting another driver",
			claims: []*resourceapi.ResourceClaim{
				newClaim(NameRef{Name: "c", Namespace: "default"},
					withResult(DeviceRef{Request: "req-1", Driver: "test-driver", Pool: "test-pool", Device: "dev-1"}),
					withImageConfig(t, ImageRef{
						Source:        "test-source",
						Driver:        DriverName,
						ContainerName: "test-container",
						Image:         "custom-image:v1",
						Requests:      []string{"req-1"},
					}),
					// Config for a different driver: must be ignored, even if its
					// payload is not decodable by this controller.
					func(c *resourceapi.ResourceClaim) {
						c.Status.Allocation.Devices.Config = append(
							c.Status.Allocation.Devices.Config,
							resourceapi.DeviceAllocationConfiguration{
								DeviceConfiguration: resourceapi.DeviceConfiguration{
									Opaque: &resourceapi.OpaqueDeviceConfiguration{
										Driver:     "other.example.com",
										Parameters: runtime.RawExtension{Raw: []byte("not-valid-json")},
									},
								},
							},
						)
					},
				),
			},
			wantLen:           1,
			wantContainerName: "test-container",
			wantImage:         "custom-image:v1",
		},
		{
			name: "returns TerminalError in case of missing ContainerName or Image in ImageConfig",
			claims: []*resourceapi.ResourceClaim{
				newClaim(NameRef{Name: "c", Namespace: "default"},
					withResult(DeviceRef{Request: "req-1", Driver: "test-driver", Pool: "test-pool", Device: "dev-1"}),
					withImageConfig(t, ImageRef{
						Source:        "invalid-source",
						Driver:        DriverName,
						ContainerName: "",
						Image:         "custom-image:v1",
						Requests:      []string{"req-1"},
					}),
				),
			},
			wantLen:     0,
			wantRequeue: false,
			errMsg:      "ContainerName or Image empty",
		},
		{
			name: "returns TerminalError in case of multiple ImageConfigs target the same container",
			claims: []*resourceapi.ResourceClaim{
				newClaim(NameRef{Name: "c", Namespace: "default"},
					withResult(DeviceRef{Request: "req-1", Driver: "test-driver", Pool: "test-pool", Device: "dev-1"}),
					withImageConfig(t, ImageRef{
						Source:        "test-source",
						Driver:        DriverName,
						ContainerName: "test-container",
						Image:         "custom-image:v1",
						Requests:      []string{"req-1"},
					}),
				),
				newClaim(NameRef{Name: "c", Namespace: "default"},
					withResult(DeviceRef{Request: "req-2", Driver: "test-driver", Pool: "test-pool", Device: "dev-2"}),
					withImageConfig(t, ImageRef{
						Source:        "test-source",
						Driver:        DriverName,
						ContainerName: "test-container",
						Image:         "custom-image:v2",
						Requests:      []string{"req-2"},
					}),
				),
			},
			wantLen:     0,
			wantRequeue: false,
			errMsg:      "conflicting ImageConfigs for container",
		},
		{
			name: "returns TerminalError in case of syntactically incorrect image reference",
			claims: []*resourceapi.ResourceClaim{
				newClaim(NameRef{Name: "c", Namespace: "default"},
					withResult(DeviceRef{Request: "req-1", Driver: "test-driver", Pool: "test-pool", Device: "dev-1"}),
					withImageConfig(t, ImageRef{
						Source:        "invalid-source",
						Driver:        DriverName,
						ContainerName: "test-container",
						Image:         "custom-image: v1",
						Requests:      []string{"req-1"},
					}),
				),
			},
			wantLen:     0,
			wantRequeue: false,
			errMsg:      "invalid image reference",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			configs, err := (&PodReconciler{}).collectImageConfigs(tc.claims)
			if len(tc.errMsg) > 0 {
				if err == nil || !strings.Contains(err.Error(), tc.errMsg) {
					t.Fatalf("expected error %v, got %v", tc.errMsg, err)
				}
				if !tc.wantRequeue && !errors.Is(err, reconcile.TerminalError(nil)) {
					t.Fatalf("expected TerminalError, got %v", err)
				}
			} else {
				if err != nil {
					t.Fatalf("collectImageConfigs failed: %v", err)
				}
			}
			if len(configs) != tc.wantLen {
				t.Fatalf("expected %d image config(s), got %d", tc.wantLen, len(configs))
			}
			if tc.wantLen > 0 {
				if configs[0].ContainerName != tc.wantContainerName {
					t.Errorf("ContainerName = %q, want %q", configs[0].ContainerName, tc.wantContainerName)
				}
				if configs[0].Image != tc.wantImage {
					t.Errorf("Image = %q, want %q", configs[0].Image, tc.wantImage)
				}
			}
		})
	}
}

// ── fetchClaims ───────────────────────────────────────────────────────────────

func TestFetchClaims(t *testing.T) {
	s := createTestScheme()
	claimName := "test-claim"

	tests := []struct {
		name    string
		pod     *corev1.Pod
		claims  []client.Object
		wantLen int
		wantErr bool
	}{
		{
			name: "fetches allocated claim referenced by pod",
			pod:  newPod(NameRef{Name: "test-pod", Namespace: "default"}, withClaimRef(claimName)),
			claims: []client.Object{
				newClaim(NameRef{Name: claimName, Namespace: "default"}, func(c *resourceapi.ResourceClaim) {
					c.Status.Allocation = &resourceapi.AllocationResult{}
				}),
			},
			wantLen: 1,
		},
		{
			name:    "Test claim not found",
			pod:     newPod(NameRef{Name: "test-pod", Namespace: "default"}, withClaimRef(claimName)),
			claims:  nil,
			wantErr: true,
		},
		{
			name: "Test claim not allocated",
			pod:  newPod(NameRef{Name: "test-pod", Namespace: "default"}, withClaimRef(claimName)),
			claims: []client.Object{
				newClaim(NameRef{Name: claimName, Namespace: "default"}), // no Allocation
			},
			wantErr: true,
		},
		{
			name: "fetches static allocated claim referenced by pod",
			pod: newPod(NameRef{Name: "test-pod", Namespace: "default"}, func(p *corev1.Pod) {
				p.Spec.ResourceClaims = append(
					p.Spec.ResourceClaims,
					corev1.PodResourceClaim{Name: "ref", ResourceClaimName: &claimName},
				)
			}), // no status.resourceClaimStatuses
			claims: []client.Object{
				newClaim(NameRef{Name: claimName, Namespace: "default"}, func(c *resourceapi.ResourceClaim) {
					c.Status.Allocation = &resourceapi.AllocationResult{}
				}),
			},
			wantLen: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(tc.claims...).Build()
			r := &PodReconciler{Client: fakeClient}

			claims, err := r.fetchClaims(context.Background(), tc.pod)
			if (err != nil) != tc.wantErr {
				t.Fatalf("fetchClaims() error = %v, wantErr %v", err, tc.wantErr)
			}
			if !tc.wantErr {
				if len(claims) != tc.wantLen {
					t.Fatalf("expected %d claim(s), got %d", tc.wantLen, len(claims))
				}
				if claims[0].Name != claimName {
					t.Errorf("expected claim name %q, got %q", claimName, claims[0].Name)
				}
			}
		})
	}
}

// ── patchImages ───────────────────────────────────────────────────────────────

func TestPatchImages(t *testing.T) {
	s := createTestScheme()

	tests := []struct {
		name                string
		pod                 *corev1.Pod
		imageConfigs        []*imagev1alpha1.ImageConfig
		wantImages          []string
		wantInitImages      []string
		errMsg              string
		wantRequeue         bool
		wantPodUpdateCalled bool
	}{
		{
			name: "returns a TerminalError in case containerName from ImageConfig does not match any pod container",
			pod: newPod(NameRef{Name: "test-pod", Namespace: "test-ns"},
				withContainer(ImageRef{ContainerName: "target-container", Image: "old-image:v1"}),
				withContainer(ImageRef{ContainerName: "other-container", Image: "other-image:v1"}),
			),
			imageConfigs: []*imagev1alpha1.ImageConfig{
				newImageConfig(ImageRef{ContainerName: "non-matching-container", Image: "new-image:v2"}),
				newImageConfig(ImageRef{ContainerName: "target-container", Image: "new-image:v1"}),
			},
			wantImages:          []string{"old-image:v1", "other-image:v1"},
			errMsg:              "containerName non-matching-container in ImageConfig doesn't match any container in pod test-ns/test-pod",
			wantRequeue:         false,
			wantPodUpdateCalled: false,
		},
		{
			name: "patches container image and init container image according to ImageConfigs",
			pod: newPod(NameRef{Name: "test-pod", Namespace: "test-ns"},
				withContainer(ImageRef{ContainerName: "target-container", Image: "old-image:v1"}),
				func(p *corev1.Pod) {
					p.Spec.InitContainers = append(p.Spec.InitContainers, corev1.Container{
						Name:  "init-container",
						Image: "init-image:v1",
					})
				},
			),
			imageConfigs: []*imagev1alpha1.ImageConfig{
				newImageConfig(ImageRef{ContainerName: "target-container", Image: "new-image:v2"}),
				newImageConfig(ImageRef{ContainerName: "init-container", Image: "new-init-image:v1"}),
			},
			wantImages:          []string{"new-image:v2"},
			wantInitImages:      []string{"new-init-image:v1"},
			wantPodUpdateCalled: true,
		},
		{
			name: "doesn't update images if they are already set to the desired value",
			pod: newPod(NameRef{Name: "test-pod", Namespace: "test-ns"},
				withContainer(ImageRef{ContainerName: "target-container", Image: "new-image:v2"}),
			),
			imageConfigs: []*imagev1alpha1.ImageConfig{
				newImageConfig(ImageRef{ContainerName: "target-container", Image: "new-image:v2"}),
			},
			wantImages:          []string{"new-image:v2"},
			wantPodUpdateCalled: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			podUpdateCalled := false
			fakeClient := fake.NewClientBuilder().
				WithScheme(s).
				WithObjects(tc.pod).
				WithInterceptorFuncs(interceptor.Funcs{
					Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
						if _, ok := obj.(*corev1.Pod); ok {
							podUpdateCalled = true
						}
						return c.Update(ctx, obj, opts...)
					},
				}).
				Build()
			r := &PodReconciler{Client: fakeClient}

			err := r.patchImages(context.Background(), tc.pod, tc.imageConfigs)
			if len(tc.errMsg) > 0 {
				if err == nil || !strings.Contains(err.Error(), tc.errMsg) {
					t.Fatalf("expected error %v, got %v", tc.errMsg, err)
				}
				if !tc.wantRequeue && !errors.Is(err, reconcile.TerminalError(nil)) {
					t.Fatalf("expected TerminalError, got %v", err)
				}
			} else {
				if err != nil {
					t.Fatalf("Reconcile failed: %v", err)
				}
			}

			// Verify whether pod Update was called as expected.
			if podUpdateCalled != tc.wantPodUpdateCalled {
				t.Errorf("pod Update called = %v, expected %v", podUpdateCalled, tc.wantPodUpdateCalled)
			}
			// Get current state of the pod
			updatedPod := &corev1.Pod{}
			if err := fakeClient.Get(context.Background(), types.NamespacedName{Namespace: tc.pod.Namespace, Name: tc.pod.Name}, updatedPod); err != nil {
				t.Fatalf("failed to get updated pod: %v", err)
			}
			// Verify the pod's container images
			for i, container := range updatedPod.Spec.Containers {
				if container.Image != tc.wantImages[i] {
					t.Errorf("expected image %v, got %v", tc.wantImages[i], container.Image)
				}
			}
			// Verify the pod's init container images
			for i, container := range updatedPod.Spec.InitContainers {
				if container.Image != tc.wantInitImages[i] {
					t.Errorf("expected init image %v, got %v", tc.wantInitImages[i], container.Image)
				}
			}
		})
	}
}

// ── Reconcile ─────────────────────────────────────────────────────────────────

func TestReconcile(t *testing.T) {
	s := createTestScheme()
	claimName := "reconcile-claim"

	tests := []struct {
		name             string
		pod              *corev1.Pod
		claim            *resourceapi.ResourceClaim
		wantImages       []string
		wantConditionTyp string
		errMsg           string
		wantRequeue      bool
	}{
		{
			name: "patches container image and sets binding condition",
			pod: newPod(NameRef{Name: "reconcile-pod", Namespace: "test-ns"},
				withContainer(ImageRef{ContainerName: "target-container", Image: "old-image:v1"}),
				withContainer(ImageRef{ContainerName: "other-container", Image: "other-image:v1"}),
				withClaimRef(claimName),
			),
			claim: newClaim(NameRef{Name: claimName, Namespace: "test-ns"},
				withImageConfig(t, ImageRef{
					Driver:        DriverName,
					ContainerName: "target-container",
					Image:         "new-image:v2",
					Requests:      []string{"req-1"},
				}),
				withResult(DeviceRef{Request: "req-1", Driver: "test-driver", Pool: "test-pool", Device: "dev-1"}),
				withResult(DeviceRef{
					Request: "req-2", Driver: DriverName, Pool: "test-pool", Device: "test-device",
					BindingConditions: []string{BindingConditionUpdateImage},
				}),
			),
			wantImages:       []string{"new-image:v2", "other-image:v1"},
			wantConditionTyp: BindingConditionUpdateImage,
		},
		{
			name: "returns an error and reconcile stops in case of no image configs",
			pod: newPod(NameRef{Name: "pod-no-configs", Namespace: "test-ns"},
				withClaimRef(claimName),
			),
			claim: newClaim(NameRef{Name: claimName, Namespace: "test-ns"},
				withResult(DeviceRef{
					Request:           "req-1",
					Driver:            DriverName,
					BindingConditions: []string{BindingConditionUpdateImage},
				}),
			),
			errMsg:      "no valid ImageConfig found in claims matching the pod containers",
			wantRequeue: false,
		},
		{
			name: "nothing happens if claim has no pending binding results",
			pod: newPod(NameRef{Name: "pod-no-image-config", Namespace: "test-ns"},
				withClaimRef(claimName),
			),
			claim: newClaim(NameRef{Name: claimName, Namespace: "test-ns"}, func(c *resourceapi.ResourceClaim) {
				c.Status.Allocation = &resourceapi.AllocationResult{}
			}),
		},
		{
			name: "nothing happens if Pod has DeletionTimestamp set",
			pod: newPod(NameRef{Name: "pod-deleting", Namespace: "test-ns"},
				withContainer(ImageRef{ContainerName: "target-container", Image: "old-image:v1"}),
				withClaimRef(claimName),
				func(p *corev1.Pod) {
					now := metav1.Now()
					p.DeletionTimestamp = &now
					p.Finalizers = []string{"test-finalizer"}
				},
			),
			claim: newClaim(NameRef{Name: claimName, Namespace: "test-ns"},
				withImageConfig(t, ImageRef{
					Driver:        DriverName,
					ContainerName: "target-container",
					Image:         "new-image:v2",
					Requests:      []string{"req-1"},
				}),
				withResult(DeviceRef{Request: "req-1", Driver: "test-driver", Pool: "test-pool", Device: "dev-1"}),
				withResult(DeviceRef{
					Request: "req-2", Driver: DriverName, Pool: "test-pool", Device: "test-device",
					BindingConditions: []string{BindingConditionUpdateImage},
				}),
			),
			wantImages: []string{"old-image:v1"},
		},
		{
			name: "nothing happens if claims result has different driver",
			pod: newPod(NameRef{Name: "pod-no-image-config", Namespace: "test-ns"},
				withContainer(ImageRef{ContainerName: "target-container", Image: "old-image:v1"}),
				withClaimRef(claimName),
			),
			//only Claim with different driver is present so it should be ignored
			claim: newClaim(NameRef{Name: claimName, Namespace: "test-ns"},
				withImageConfig(t, ImageRef{
					Driver:        "other-driver",
					ContainerName: "target-container",
					Image:         "new-image:v2",
					Requests:      []string{"req-1"},
				}),
				withResult(DeviceRef{
					Request: "req-1", Driver: "other-driver", Pool: "test-pool", Device: "test-device",
					BindingConditions: []string{"other-binding-condition"},
				}),
			),
			wantImages: []string{"old-image:v1"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fakeClient := fake.NewClientBuilder().
				WithScheme(s).
				WithObjects(tc.pod, tc.claim).
				WithStatusSubresource(&resourceapi.ResourceClaim{}).
				Build()
			reconciler := &PodReconciler{Client: fakeClient}

			req := reconcile.Request{
				NamespacedName: types.NamespacedName{
					Namespace: tc.pod.Namespace,
					Name:      tc.pod.Name,
				},
			}

			// Run Reconcile
			res, err := reconciler.Reconcile(context.Background(), req)
			if len(tc.errMsg) > 0 {
				if err == nil || !strings.Contains(err.Error(), tc.errMsg) {
					t.Fatalf("expected error %v, got %v", tc.errMsg, err)
				}
				if !tc.wantRequeue && !errors.Is(err, reconcile.TerminalError(nil)) {
					t.Fatalf("expected TerminalError, got %v", err)
				}
			} else {
				if err != nil {
					t.Fatalf("Reconcile failed: %v", err)
				}
			}
			if tc.wantRequeue {
				if res == (reconcile.Result{}) {
					t.Errorf("expected Requeue, got none")
				}
			} else {
				if res != (reconcile.Result{}) {
					t.Errorf("unexpected Requeue")
				}
			}

			// Verify pod images were updated as expected.
			updatedPod := &corev1.Pod{}
			if err := fakeClient.Get(context.Background(), req.NamespacedName, updatedPod); err != nil {
				t.Fatalf("failed to get updated pod: %v", err)
			}

			for i, want := range tc.wantImages {
				if got := updatedPod.Spec.Containers[i].Image; got != want {
					t.Errorf("container %d image = %q, want %q", i, got, want)
				}
			}

			// Verify ResourceClaim status was updated with the binding condition.
			if tc.wantConditionTyp == "" {
				return
			}
			updatedClaim := &resourceapi.ResourceClaim{}
			if err := fakeClient.Get(context.Background(), types.NamespacedName{Namespace: tc.claim.Namespace, Name: tc.claim.Name}, updatedClaim); err != nil {
				t.Fatalf("failed to get updated claim: %v", err)
			}

			if len(updatedClaim.Status.Devices) != 1 {
				t.Fatalf("expected 1 allocated device status in claim, got %d", len(updatedClaim.Status.Devices))
			}
			if len(updatedClaim.Status.Devices[0].Conditions) != 1 {
				t.Fatalf("expected 1 condition in allocated device status, got %d", len(updatedClaim.Status.Devices[0].Conditions))
			}
			if updatedClaim.Status.Devices[0].Conditions[0].Type != tc.wantConditionTyp {
				t.Errorf("expected condition type %q, got %q", tc.wantConditionTyp, updatedClaim.Status.Devices[0].Conditions[0].Type)
			}
			if updatedClaim.Status.Devices[0].Conditions[0].Status != metav1.ConditionTrue {
				t.Errorf("expected condition status True, got %v", updatedClaim.Status.Devices[0].Conditions[0].Status)
			}

			// Re-running Reconcile should now do nothing since binding condition is already set.
			res, err = reconciler.Reconcile(context.Background(), req)
			if err != nil {
				t.Fatalf("second Reconcile failed: %v", err)
			}
			if res != (reconcile.Result{}) {
				t.Errorf("unexpected Requeue on second Reconcile")
			}
		})
	}
}

func TestReconcile_PodNotFound(t *testing.T) {
	s := createTestScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(s).Build()
	reconciler := &PodReconciler{Client: fakeClient}

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Namespace: "test-ns",
			Name:      "non-existent-pod",
		},
	}

	res, err := reconciler.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("expected no error for non-existent pod, got %v", err)
	}
	if res != (reconcile.Result{}) {
		t.Errorf("unexpected Requeue")
	}
}

func TestReconcile_APIErrors(t *testing.T) {
	s := createTestScheme()
	claimName := "reconcile-claim"
	pod := newPod(NameRef{Name: "reconcile-pod", Namespace: "test-ns"},
		withContainer(ImageRef{ContainerName: "target-container", Image: "old-image:v1"}),
		withClaimRef(claimName),
	)
	claim := newClaim(NameRef{Name: claimName, Namespace: "test-ns"},
		withImageConfig(t, ImageRef{
			Driver:        DriverName,
			ContainerName: "target-container",
			Image:         "new-image:v2",
			Requests:      []string{"req-1"},
		}),
		withResult(DeviceRef{Request: "req-1", Driver: "test-driver", Pool: "test-pool", Device: "dev-1"}),
		withResult(DeviceRef{
			Request: "req-2", Driver: DriverName, Pool: "test-pool", Device: "test-device",
			BindingConditions: []string{BindingConditionUpdateImage},
		}),
	)

	tests := []struct {
		name            string
		interceptorFunc interceptor.Funcs
		errMsg          string
	}{
		{
			name: "returns error and requeues on API error while fetching pod",
			interceptorFunc: interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if _, ok := obj.(*corev1.Pod); ok {
						return apierrors.NewServiceUnavailable("simulated API server unavailable when fetching pod")
					}
					return c.Get(ctx, key, obj, opts...)
				},
			},
			errMsg: "simulated API server unavailable when fetching pod",
		},
		{
			name: "returns error and requeues on API error while fetching claim",
			interceptorFunc: interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if _, ok := obj.(*resourceapi.ResourceClaim); ok {
						return apierrors.NewServiceUnavailable("simulated API server unavailable when fetching claim")
					}
					return c.Get(ctx, key, obj, opts...)
				},
			},
			errMsg: "simulated API server unavailable when fetching claim",
		},
		{
			name: "returns error and requeues on API error while updating Pod with new image",
			interceptorFunc: interceptor.Funcs{
				Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					if _, ok := obj.(*corev1.Pod); ok {
						return apierrors.NewForbidden(corev1.Resource("pods"), "test-pod", fmt.Errorf("updating Pod with new image forbidden"))
					}
					return c.Update(ctx, obj, opts...)
				},
			},
			errMsg: "updating Pod with new image forbidden",
		},
		{
			name: "returns error and requeues on API error while updating claim status with binding condition",
			interceptorFunc: interceptor.Funcs{
				SubResourceUpdate: func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
					if _, ok := obj.(*resourceapi.ResourceClaim); ok {
						return apierrors.NewConflict(resourceapi.Resource("resourceclaims"), "test-claim", fmt.Errorf("resource version mismatch when updating claim status with binding condition"))
					}
					return c.SubResource(subResourceName).Update(ctx, obj, opts...)
				},
			},
			errMsg: "resource version mismatch when updating claim status with binding condition",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fakeClient := fake.NewClientBuilder().
				WithScheme(s).
				WithObjects(pod, claim).
				WithStatusSubresource(&resourceapi.ResourceClaim{}).
				WithInterceptorFuncs(tc.interceptorFunc).
				Build()
			reconciler := &PodReconciler{Client: fakeClient}

			req := reconcile.Request{
				NamespacedName: types.NamespacedName{
					Namespace: pod.Namespace,
					Name:      pod.Name,
				},
			}
			_, err := reconciler.Reconcile(context.Background(), req)
			if err == nil || errors.Is(err, reconcile.TerminalError(nil)) {
				t.Fatalf("expected regular error containing %q, got %v", tc.errMsg, err)
			}
			if !strings.Contains(err.Error(), tc.errMsg) {
				t.Errorf("expected error to contain %q, got %q", tc.errMsg, err.Error())
			}
		})
	}
}
