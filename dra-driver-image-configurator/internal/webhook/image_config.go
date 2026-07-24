package webhook

import (
	"context"
	"fmt"
	"net/http"

	resourceapi "k8s.io/api/resource/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	imagev1alpha1 "github.com/gke-labs/dra-drivers/dra-driver-image-configurator/api/v1alpha1"
)

type ResourceClaimValidator struct {
	Decoder admission.Decoder
}

func (v *ResourceClaimValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
	claim := &resourceapi.ResourceClaim{}
	if err := v.Decoder.Decode(req, claim); err != nil {
		return admission.Errored(http.StatusBadRequest, fmt.Errorf("decode ResourceClaim: %w", err))
	}
	if err := validateClaimConfigs(claim.Spec.Devices.Config); err != nil {
		return admission.Denied(err.Error())
	}
	return admission.Allowed("")
}

type ResourceClaimTemplateValidator struct {
	Decoder admission.Decoder
}

func (v *ResourceClaimTemplateValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
	template := &resourceapi.ResourceClaimTemplate{}
	if err := v.Decoder.Decode(req, template); err != nil {
		return admission.Errored(http.StatusBadRequest, fmt.Errorf("decode ResourceClaimTemplate: %w", err))
	}
	if err := validateClaimConfigs(template.Spec.Spec.Devices.Config); err != nil {
		return admission.Denied(err.Error())
	}
	return admission.Allowed("")
}

func validateClaimConfigs(configs []resourceapi.DeviceClaimConfiguration) error {
	for _, cfg := range configs {
		_, err := imagev1alpha1.DecodeAndValidateOpaque(cfg.Opaque)
		if err != nil {
			return err
		}
	}
	return nil
}
