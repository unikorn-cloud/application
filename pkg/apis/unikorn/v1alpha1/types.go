/*
Copyright 2024 the Unikorn Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	unikornv1core "github.com/unikorn-cloud/core/pkg/apis/unikorn/v1alpha1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ApplicationSetList defines a list of Helm applications.
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ApplicationSetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ApplicationSet `json:"items"`
}

// ApplicationSet defines a Helm application.
// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:resource:scope=Cluster,categories=unikorn
// +kubebuilder:printcolumn:name="display name",type="string",JSONPath=".metadata.labels['unikorn-cloud\\.org/name']"
// +kubebuilder:printcolumn:name="age",type="date",JSONPath=".metadata.creationTimestamp"
type ApplicationSet struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ApplicationSetSpec   `json:"spec"`
	Status            ApplicationSetStatus `json:"status,omitempty"`
}

type ApplicationSetSpec struct {
	// Pause, if true, will inhibit reconciliation.
	Pause bool `json:"pause,omitempty"`
}

type ApplicationSetStatus struct {
	// Current service state of the resource.
	Conditions []unikornv1core.Condition `json:"conditions,omitempty"`
}
