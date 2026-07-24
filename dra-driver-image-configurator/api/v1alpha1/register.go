package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
)

var (
	SchemeGroupVersion = schema.GroupVersion{Group: "image-configurator.x-k8s.io", Version: "v1alpha1"}
	SchemeBuilder      = runtime.NewSchemeBuilder(addKnownTypes)
	AddToScheme        = SchemeBuilder.AddToScheme

	scheme  = runtime.NewScheme()
	codec   = serializer.NewCodecFactory(scheme)
	decoder = codec.UniversalDeserializer()
)

const (
	DriverName = "image-configurator.x-k8s.io"
)

func init() {
	utilruntime.Must(AddToScheme(scheme))
}

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(SchemeGroupVersion, &ImageConfig{})
	return nil
}

func (in *ImageConfig) DeepCopyObject() runtime.Object {
	return &ImageConfig{
		TypeMeta:      in.TypeMeta,
		ContainerName: in.ContainerName,
		Image:         in.Image,
	}
}
