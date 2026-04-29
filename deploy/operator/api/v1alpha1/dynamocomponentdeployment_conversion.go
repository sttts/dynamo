/*
 * SPDX-FileCopyrightText: Copyright (c) 2025-2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

// Conversion between v1alpha1 and v1beta1 DynamoComponentDeployment.
// See dynamographdeployment_conversion.go for the design rationale.

package v1alpha1

import (
	"encoding/json"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/conversion"

	v1beta1 "github.com/ai-dynamo/dynamo/deploy/operator/api/v1beta1"
)

const (
	annDCDHubSpec     = "nvidia.com/dcd-hub-spec"
	annDCDSpokeSpec   = "nvidia.com/dcd-spoke-spec"
	annDCDSpokeStatus = "nvidia.com/dcd-spoke-status"
	annDCDSpokeHub    = "nvidia.com/dcd-spoke-hub"
	annDCDHubOrigin   = "nvidia.com/dcd-hub-origin"
)

// ConvertTo converts this DynamoComponentDeployment (v1alpha1) into the hub
// version (v1beta1).
func (src *DynamoComponentDeployment) ConvertTo(dstRaw conversion.Hub) error {
	dst, ok := dstRaw.(*v1beta1.DynamoComponentDeployment)
	if !ok {
		return fmt.Errorf("expected *v1beta1.DynamoComponentDeployment but got %T", dstRaw)
	}

	dst.ObjectMeta = *src.ObjectMeta.DeepCopy()
	restoredHubSpec := false

	if raw, ok := dst.ObjectMeta.Annotations[annDCDHubSpec]; ok && raw != "" {
		if spec, ok := restoreDynamoComponentDeploymentHubSpec(raw); ok {
			dst.Spec = spec
			restoredHubSpec = true
			delAnnFromObj(&dst.ObjectMeta, annDCDHubSpec)
		}
	}
	hubOrigin := restoredHubSpec || dst.ObjectMeta.Annotations[annDCDHubOrigin] == annotationTrue
	delAnnFromObj(&dst.ObjectMeta, annDCDHubOrigin)

	var semantic v1beta1.DynamoComponentDeploymentSpec
	semantic.BackendFramework = src.Spec.BackendFramework
	carrier := newDynamoComponentDeploymentCarrier(&dst.ObjectMeta)
	if err := convertDynamoComponentDeploymentSharedSpecTo(&src.Spec.DynamoComponentDeploymentSharedSpec,
		&semantic.DynamoComponentDeploymentSharedSpec, carrier); err != nil {
		return err
	}

	// v1beta1 requires DCD.spec.name (it is the +listMapKey on
	// DGD.spec.components and is enforced as Required by the schema). When a
	// v1alpha1 caller omits ServiceName -- the common case for standalone
	// DCDs -- fall back to ObjectMeta.Name so the converted object is
	// schema-valid. The v1beta1 defaulting webhook owns the same defaulting
	// at admission time.
	if semantic.ComponentName == "" && !hubOrigin {
		semantic.ComponentName = dst.ObjectMeta.Name
	}
	if restoredHubSpec {
		overlayDynamoComponentDeploymentHubSpec(&dst.Spec, &semantic)
	} else {
		dst.Spec = semantic
	}

	preserveSpoke := !hubOrigin || dynamoComponentDeploymentSharedSpecHasAlphaOnlyFields(&src.Spec.DynamoComponentDeploymentSharedSpec)
	if hubOrigin {
		scrubDynamoComponentDeploymentAnnotations(&dst.ObjectMeta)
	}
	convertDynamoComponentDeploymentStatusTo(&src.Status, &dst.Status)
	if preserveSpoke {
		preserveDynamoComponentDeploymentSpoke(src, dst)
		preserveDynamoComponentDeploymentSpokeHub(dst)
	}
	return nil
}

func preserveDynamoComponentDeploymentSpoke(src *DynamoComponentDeployment, dst *v1beta1.DynamoComponentDeployment) {
	if data, err := marshalDynamoComponentDeploymentSpokeSpec(&src.Spec); err == nil {
		if dst.ObjectMeta.Annotations == nil {
			dst.ObjectMeta.Annotations = map[string]string{}
		}
		dst.ObjectMeta.Annotations[annDCDSpokeSpec] = string(data)
	}
	if data, err := json.Marshal(src.Status); err == nil {
		if dst.ObjectMeta.Annotations == nil {
			dst.ObjectMeta.Annotations = map[string]string{}
		}
		dst.ObjectMeta.Annotations[annDCDSpokeStatus] = string(data)
	}
}

func overlayDynamoComponentDeploymentHubSpec(base *v1beta1.DynamoComponentDeploymentSpec, semantic *v1beta1.DynamoComponentDeploymentSpec) {
	hubPodTemplate := base.PodTemplate
	hubFrontendSidecar := base.FrontendSidecar
	hubExperimental := base.Experimental

	*base = *semantic.DeepCopy()
	if hubPodTemplate != nil {
		base.PodTemplate = hubPodTemplate
	}
	if base.FrontendSidecar == nil {
		base.FrontendSidecar = hubFrontendSidecar
	}
	if base.Experimental == nil {
		base.Experimental = hubExperimental
	}
}

func restoreDynamoComponentDeploymentSpokeFromPreserved(dstSpec *DynamoComponentDeploymentSpec, dstStatus *DynamoComponentDeploymentStatus, preservedSpec *DynamoComponentDeploymentSpec, preservedStatus *DynamoComponentDeploymentStatus) {
	if preservedSpec != nil {
		restoreDynamoComponentDeploymentSharedSpecAlphaOnlyFields(&dstSpec.DynamoComponentDeploymentSharedSpec, &preservedSpec.DynamoComponentDeploymentSharedSpec)
	}
	if preservedStatus != nil && dstStatus.Service != nil && preservedStatus.Service != nil && dstStatus.Service.ComponentName == "" {
		dstStatus.Service.ComponentName = preservedStatus.Service.ComponentName
	}
}

type preservedDynamoComponentDeploymentHubSnapshot struct {
	Spec   string                                  `json:"spec"`
	Status v1beta1.DynamoComponentDeploymentStatus `json:"status"`
}

func preserveDynamoComponentDeploymentSpokeHub(dst *v1beta1.DynamoComponentDeployment) {
	spec, err := marshalDynamoComponentDeploymentHubSpec(&dst.Spec)
	if err != nil {
		return
	}
	data, err := json.Marshal(preservedDynamoComponentDeploymentHubSnapshot{
		Spec:   string(spec),
		Status: dst.Status,
	})
	if err == nil {
		setAnnOnObj(&dst.ObjectMeta, annDCDSpokeHub, string(data))
	}
}

func dynamoComponentDeploymentSpokeHubUnmodified(src *v1beta1.DynamoComponentDeployment) bool {
	raw, ok := src.ObjectMeta.Annotations[annDCDSpokeHub]
	if !ok || raw == "" {
		return false
	}
	spec, err := marshalDynamoComponentDeploymentHubSpec(&src.Spec)
	if err != nil {
		return false
	}
	current, err := json.Marshal(preservedDynamoComponentDeploymentHubSnapshot{
		Spec:   string(spec),
		Status: src.Status,
	})
	if err != nil {
		return false
	}
	return string(current) == raw
}

// ConvertFrom converts from the hub (v1beta1) DynamoComponentDeployment into
// this v1alpha1 instance.
func (dst *DynamoComponentDeployment) ConvertFrom(srcRaw conversion.Hub) error {
	src, ok := srcRaw.(*v1beta1.DynamoComponentDeployment)
	if !ok {
		return fmt.Errorf("expected *v1beta1.DynamoComponentDeployment but got %T", srcRaw)
	}

	dst.ObjectMeta = *src.ObjectMeta.DeepCopy()
	dst.Spec.BackendFramework = src.Spec.BackendFramework

	var preservedSpokeSpec *DynamoComponentDeploymentSpec
	var preservedSpokeStatus *DynamoComponentDeploymentStatus
	if raw, ok := dst.ObjectMeta.Annotations[annDCDSpokeSpec]; ok && raw != "" {
		if spec, ok := restoreDynamoComponentDeploymentSpokeSpec(raw); ok {
			preservedSpokeSpec = &spec
		}
	}
	if rawStatus, ok := dst.ObjectMeta.Annotations[annDCDSpokeStatus]; ok && rawStatus != "" {
		var status DynamoComponentDeploymentStatus
		if err := json.Unmarshal([]byte(rawStatus), &status); err == nil {
			preservedSpokeStatus = &status
		}
	}
	// Fast path only: the fingerprint covers the hub spec/status snapshot, so
	// matching means no hub fields changed. Metadata was copied above and rides along.
	if preservedSpokeSpec != nil && dynamoComponentDeploymentSpokeHubUnmodified(src) {
		dst.Spec = *preservedSpokeSpec.DeepCopy()
		if preservedSpokeStatus != nil {
			dst.Status = *preservedSpokeStatus.DeepCopy()
		} else {
			convertDynamoComponentDeploymentStatusFrom(&src.Status, &dst.Status)
		}
		scrubDynamoComponentDeploymentAnnotations(&dst.ObjectMeta)
		delAnnFromObj(&dst.ObjectMeta, annDCDHubOrigin)
		return nil
	}

	generatedPodTemplate := src.ObjectMeta.Annotations[annDCDPrefix+suffixPodTemplateOrig] == "generated"
	carrier := newDynamoComponentDeploymentCarrier(&dst.ObjectMeta)
	if err := convertDynamoComponentDeploymentSharedSpecFrom(&src.Spec.DynamoComponentDeploymentSharedSpec,
		&dst.Spec.DynamoComponentDeploymentSharedSpec, carrier); err != nil {
		return err
	}

	convertDynamoComponentDeploymentStatusFrom(&src.Status, &dst.Status)
	restoreDynamoComponentDeploymentSpokeFromPreserved(&dst.Spec, &dst.Status, preservedSpokeSpec, preservedSpokeStatus)
	scrubDynamoComponentDeploymentAnnotations(&dst.ObjectMeta)
	if dynamoComponentDeploymentNeedsHubSpecPreservation(&src.Spec, generatedPodTemplate) {
		data, err := marshalDynamoComponentDeploymentHubSpec(&src.Spec)
		if err != nil {
			return fmt.Errorf("preserve DCD hub spec: %w", err)
		}
		if dst.ObjectMeta.Annotations == nil {
			dst.ObjectMeta.Annotations = map[string]string{}
		}
		dst.ObjectMeta.Annotations[annDCDHubSpec] = string(data)
	} else if !hasDynamoComponentDeploymentInternalAnnotations(src.ObjectMeta.Annotations) {
		if dst.ObjectMeta.Annotations == nil {
			dst.ObjectMeta.Annotations = map[string]string{}
		}
		dst.ObjectMeta.Annotations[annDCDHubOrigin] = annotationTrue
	}
	return nil
}

func marshalDynamoComponentDeploymentHubSpec(src *v1beta1.DynamoComponentDeploymentSpec) ([]byte, error) {
	return marshalPreservedSpec(*src.DeepCopy(), func(spec *v1beta1.DynamoComponentDeploymentSpec, records *[]preservedRawJSON) {
		if spec.EPPConfig != nil {
			preserveEPPPluginParameters(spec.EPPConfig.Config, "eppConfig/config", records)
		}
	})
}

func restoreDynamoComponentDeploymentHubSpec(raw string) (v1beta1.DynamoComponentDeploymentSpec, bool) {
	return restorePreservedSpec(raw, func(spec *v1beta1.DynamoComponentDeploymentSpec, records []preservedRawJSON) {
		if spec.EPPConfig != nil {
			restoreEPPPluginParameters(spec.EPPConfig.Config, "eppConfig/config", records)
		}
	})
}

func marshalDynamoComponentDeploymentSpokeSpec(src *DynamoComponentDeploymentSpec) ([]byte, error) {
	return marshalPreservedSpec(*src.DeepCopy(), func(spec *DynamoComponentDeploymentSpec, records *[]preservedRawJSON) {
		if spec.EPPConfig != nil {
			preserveEPPPluginParameters(spec.EPPConfig.Config, "eppConfig/config", records)
		}
	})
}

func restoreDynamoComponentDeploymentSpokeSpec(raw string) (DynamoComponentDeploymentSpec, bool) {
	return restorePreservedSpec(raw, func(spec *DynamoComponentDeploymentSpec, records []preservedRawJSON) {
		if spec.EPPConfig != nil {
			restoreEPPPluginParameters(spec.EPPConfig.Config, "eppConfig/config", records)
		}
	})
}

func dynamoComponentDeploymentNeedsHubSpecPreservation(src *v1beta1.DynamoComponentDeploymentSpec, generatedPodTemplate bool) bool {
	if generatedPodTemplate {
		return false
	}
	return src.FrontendSidecar != nil ||
		src.PodTemplate != nil ||
		(src.Experimental != nil &&
			src.Experimental.GPUMemoryService == nil &&
			src.Experimental.Failover == nil &&
			src.Experimental.Checkpoint == nil)
}

func hasDynamoComponentDeploymentInternalAnnotations(annotations map[string]string) bool {
	for key := range annotations {
		if key == annDCDHubSpec ||
			key == annDCDSpokeSpec ||
			key == annDCDSpokeStatus ||
			key == annDCDSpokeHub ||
			strings.HasPrefix(key, annDCDPrefix) {
			return true
		}
	}
	return false
}

func convertDynamoComponentDeploymentStatusTo(src *DynamoComponentDeploymentStatus, dst *v1beta1.DynamoComponentDeploymentStatus) {
	dst.ObservedGeneration = src.ObservedGeneration
	if len(src.Conditions) > 0 {
		dst.Conditions = make([]metav1.Condition, 0, len(src.Conditions))
		for _, c := range src.Conditions {
			dst.Conditions = append(dst.Conditions, *c.DeepCopy())
		}
	}
	if src.Service != nil {
		dst.Component = convertReplicaStatusTo(src.Service)
	}
	// PodSelector is dropped in v1beta1 (the field was never populated by the
	// controller). No annotation is needed: the round-trip invariant is on
	// v1beta1 inputs, which do not carry PodSelector.
}

func convertDynamoComponentDeploymentStatusFrom(src *v1beta1.DynamoComponentDeploymentStatus, dst *DynamoComponentDeploymentStatus) {
	dst.ObservedGeneration = src.ObservedGeneration
	if len(src.Conditions) > 0 {
		dst.Conditions = make([]metav1.Condition, 0, len(src.Conditions))
		for _, c := range src.Conditions {
			dst.Conditions = append(dst.Conditions, *c.DeepCopy())
		}
	}
	if src.Component != nil {
		dst.Service = convertReplicaStatusFrom(src.Component)
	}
}

// scrubDynamoComponentDeploymentAnnotations removes any lingering "nvidia.com/dcd-*" keys that
// convertDynamoComponentDeploymentSharedSpecFrom did not consume.
func scrubDynamoComponentDeploymentAnnotations(obj *metav1.ObjectMeta) {
	scrubAnnotationsByPrefix(obj, annDCDPrefix)
}
