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
	"errors"

	unikornv1core "github.com/unikorn-cloud/core/pkg/apis/unikorn/v1alpha1"
	"github.com/unikorn-cloud/core/pkg/constants"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
)

var (
	// ErrMissingLabel is raised when an expected label is not present on
	// a resource.
	ErrMissingLabel = errors.New("expected label is missing")

	// ErrApplicationLookup is raised when the named application is not
	// present in an application bundle bundle.
	ErrApplicationLookup = errors.New("failed to lookup an application")
)

// Paused implements the ReconcilePauser interface.
func (c *ApplicationSet) Paused() bool {
	return c.Spec.Pause
}

// StatusConditionRead scans the status conditions for an existing condition whose type
// matches.
func (c *ApplicationSet) StatusConditionRead(t unikornv1core.ConditionType) (*unikornv1core.Condition, error) {
	return unikornv1core.GetCondition(c.Status.Conditions, t)
}

// StatusConditionWrite either adds or updates a condition in the cluster manager status.
// If the condition, status and message match an existing condition the update is
// ignored.
func (c *ApplicationSet) StatusConditionWrite(t unikornv1core.ConditionType, status corev1.ConditionStatus, reason unikornv1core.ConditionReason, message string) {
	unikornv1core.UpdateCondition(&c.Status.Conditions, t, status, reason, message)
}

// ResourceLabels generates a set of labels to uniquely identify the resource
// if it were to be placed in a single global namespace.
func (c *ApplicationSet) ResourceLabels() (labels.Set, error) {
	organization, ok := c.Labels[constants.OrganizationLabel]
	if !ok {
		return nil, ErrMissingLabel
	}

	project, ok := c.Labels[constants.ProjectLabel]
	if !ok {
		return nil, ErrMissingLabel
	}

	cluster, ok := c.Labels[constants.KubernetesClusterLabel]
	if !ok {
		return nil, ErrMissingLabel
	}

	labels := labels.Set{
		constants.KindLabel:              "unikorn-cloud.org/applicationset",
		constants.OrganizationLabel:      organization,
		constants.ProjectLabel:           project,
		constants.KubernetesClusterLabel: cluster,
	}

	return labels, nil
}
