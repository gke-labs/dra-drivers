package testutil

import (
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	imagev1alpha1 "github.com/gke-labs/dra-drivers/dra-driver-image-configurator/api/v1alpha1"
)

type ClaimOption func(*resourceapi.ResourceClaim)

// ImageRef specifies a container image override delivered via ImageConfig.
// Source and Driver are used only when the ref is passed to withImageConfig
type ImageRef struct {
	Source        string
	Driver        string
	ContainerName string
	Image         string
	Requests      []string
	Kind          string //optional, defaults to "ImageConfig" in NewImageConfig
	ApiVersion    string //optional, defaults to "image-configurator.x-k8s.io/v1alpha1" in NewImageConfig
}

// NameRef identifies a namespaced resource by name and namespace.
type NameRef struct {
	Name      string
	Namespace string
}

// NewClaim builds a ResourceClaim with the given name/namespace.
func NewClaim(ref NameRef, opts ...ClaimOption) *resourceapi.ResourceClaim {
	c := &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: ref.Name, Namespace: ref.Namespace},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// NewImageConfig builds an ImageConfig from an ImageRef.
func NewImageConfig(ref ImageRef) *imagev1alpha1.ImageConfig {
	//default values for Kind and ApiVersion if not provided
	if ref.Kind == "" {
		ref.Kind = "ImageConfig"
	}
	if ref.ApiVersion == "" {
		ref.ApiVersion = imagev1alpha1.SchemeGroupVersion.String()
	}
	return &imagev1alpha1.ImageConfig{
		TypeMeta: metav1.TypeMeta{
			APIVersion: ref.ApiVersion,
			Kind:       ref.Kind,
		},
		ContainerName: ref.ContainerName,
		Image:         ref.Image,
	}
}
