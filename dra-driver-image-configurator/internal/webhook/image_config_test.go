package webhook

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	imagev1alpha1 "github.com/gke-labs/dra-drivers/dra-driver-image-configurator/api/v1alpha1"
	"github.com/gke-labs/dra-drivers/dra-driver-image-configurator/internal/testutil"
)

// ── builders ──────────────────────────────────────────────────────────────────

type claimTemplateOption func(*resourceapi.ResourceClaimTemplate)

func newTestDecoder(t *testing.T) admission.Decoder {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := resourceapi.AddToScheme(scheme); err != nil {
		t.Fatalf("add resourceapi to scheme: %v", err)
	}
	return admission.NewDecoder(scheme)
}

// newTemplate builds a ResourceClaimTemplate with the given name/namespace.
func newTemplate(ref testutil.NameRef, opts ...claimTemplateOption) *resourceapi.ResourceClaimTemplate {
	c := &resourceapi.ResourceClaimTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: ref.Name, Namespace: ref.Namespace},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

func withClaimSpecImageConfig(t *testing.T, imageRef testutil.ImageRef) testutil.ClaimOption {
	t.Helper()
	raw, err := json.Marshal(testutil.NewImageConfig(imageRef))
	if err != nil {
		t.Fatalf("marshal ImageConfig: %v", err)
	}
	return func(c *resourceapi.ResourceClaim) {
		c.Spec.Devices.Config = append(
			c.Spec.Devices.Config,
			resourceapi.DeviceClaimConfiguration{
				Requests: []string{"image-config"},
				DeviceConfiguration: resourceapi.DeviceConfiguration{
					Opaque: &resourceapi.OpaqueDeviceConfiguration{
						Driver:     imagev1alpha1.DriverName,
						Parameters: runtime.RawExtension{Raw: raw},
					},
				},
			},
		)
	}
}

func withTemplateSpecImageConfig(t *testing.T, imageRef testutil.ImageRef) claimTemplateOption {
	t.Helper()
	raw, err := json.Marshal(testutil.NewImageConfig(imageRef))
	if err != nil {
		t.Fatalf("marshal ImageConfig: %v", err)
	}
	return func(c *resourceapi.ResourceClaimTemplate) {
		c.Spec.Spec.Devices.Config = append(
			c.Spec.Spec.Devices.Config,
			resourceapi.DeviceClaimConfiguration{
				Requests: []string{"image-config"},
				DeviceConfiguration: resourceapi.DeviceConfiguration{
					Opaque: &resourceapi.OpaqueDeviceConfiguration{
						Driver:     imagev1alpha1.DriverName,
						Parameters: runtime.RawExtension{Raw: raw},
					},
				},
			},
		)
	}
}

func admissionRequest(t *testing.T, obj runtime.Object) admission.Request {
	t.Helper()
	raw, err := json.Marshal(obj)
	if err != nil {
		t.Fatalf("marshal object: %v", err)
	}
	return admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Object: runtime.RawExtension{Raw: raw},
		},
	}
}

// ── Helpers ───────────────────────────────────────────────────────────

func responseMessage(resp admission.Response) string {
	if resp.Result == nil {
		return ""
	}
	return resp.Result.Message
}

func assertResponse(t *testing.T, resp admission.Response, errMsg string) {
	if len(errMsg) > 0 {
		if resp.Allowed {
			t.Fatalf("Expected the request to be Denied, but it was Allowed")
		}
		if !strings.Contains(responseMessage(resp), errMsg) {
			t.Fatalf("Expected error message to contain %q, got %q", errMsg, responseMessage(resp))
		}
	} else {
		if !resp.Allowed {
			t.Fatalf("Expected the request to be Allowed, but it was Denied: %v", responseMessage(resp))
		}
	}
}

// ── Shared Test Cases ──────────────────────────────────────────────────

// imageTestCase describes a single webhook validation scenario.
type imageTestCase struct {
	name          string
	ContainerName string
	Image         string
	Kind          string
	ApiVersion    string
	errMsg        string
}

var sharedTests = []imageTestCase{
	{
		name:          "valid config is allowed",
		ContainerName: "app",
		Image:         "ubuntu:latest",
		Kind:          "ImageConfig",
		ApiVersion:    imagev1alpha1.SchemeGroupVersion.String(),
		errMsg:        "",
	},
	{
		name:          "invalid image reference is denied",
		ContainerName: "app",
		Image:         "ubuntu: latest",
		Kind:          "ImageConfig",
		ApiVersion:    imagev1alpha1.SchemeGroupVersion.String(),
		errMsg:        "invalid image reference",
	},
	{
		name:          "unexpected type is denied",
		ContainerName: "app",
		Image:         "ubuntu:latest",
		Kind:          "NotAnImageConfig",
		ApiVersion:    imagev1alpha1.SchemeGroupVersion.String(),
		errMsg:        "Opaque parameter decode failure",
	},
	{
		name:          "unexpected ApiVersion is denied",
		ContainerName: "app",
		Image:         "ubuntu:latest",
		Kind:          "ImageConfig",
		ApiVersion:    "invalid.api.version",
		errMsg:        "Opaque parameter decode failure",
	},
	{
		name:          "empty container name is denied",
		ContainerName: "",
		Image:         "ubuntu:latest",
		Kind:          "ImageConfig",
		ApiVersion:    imagev1alpha1.SchemeGroupVersion.String(),
		errMsg:        "ContainerName or Image empty",
	},
	{
		name:          "empty image is denied",
		ContainerName: "app",
		Image:         "",
		Kind:          "ImageConfig",
		ApiVersion:    imagev1alpha1.SchemeGroupVersion.String(),
		errMsg:        "ContainerName or Image empty",
	},
}

// ── ResourceClaimValidator ─────────────────────────────────────────────

func TestResourceClaimValidator(t *testing.T) {
	validator := &ResourceClaimValidator{Decoder: newTestDecoder(t)}
	// Add ResourceClaim-specific cases here; shared cases will be appended to the end of the list.
	tests := append([]imageTestCase{}, sharedTests...)
	for _, tc := range tests {
		claim := testutil.NewClaim(testutil.NameRef{Name: "test-claim", Namespace: "default"},
			withClaimSpecImageConfig(t, testutil.ImageRef{
				ContainerName: tc.ContainerName,
				Image:         tc.Image,
				Kind:          tc.Kind,
				ApiVersion:    tc.ApiVersion,
			}))
		t.Run(tc.name, func(t *testing.T) {
			resp := validator.Handle(context.Background(), admissionRequest(t, claim))
			assertResponse(t, resp, tc.errMsg)
		})
	}
}

// ── ResourceClaimTemplateValidator ─────────────────────────────────────────────

func TestResourceClaimTemplateValidator(t *testing.T) {
	validator := &ResourceClaimTemplateValidator{Decoder: newTestDecoder(t)}
	// Add ResourceClaimTemplate-specific cases here; shared cases will be appended to the end of the list.
	tests := append([]imageTestCase{}, sharedTests...)
	for _, tc := range tests {
		template := newTemplate(testutil.NameRef{Name: "test-template", Namespace: "default"},
			withTemplateSpecImageConfig(t, testutil.ImageRef{
				ContainerName: tc.ContainerName,
				Image:         tc.Image,
				Kind:          tc.Kind,
				ApiVersion:    tc.ApiVersion,
			}))
		t.Run(tc.name, func(t *testing.T) {
			resp := validator.Handle(context.Background(), admissionRequest(t, template))
			assertResponse(t, resp, tc.errMsg)
		})
	}
}
