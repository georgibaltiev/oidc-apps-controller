// Copyright 2024 SAP SE or an SAP affiliate company. All rights reserved. This file is licensed under the Apache Software License, v. 2 except as noted otherwise in the LICENSE file.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controllers

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	oidc_apps_controller "github.com/gardener/oidc-apps-controller/pkg/constants"

	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	gardenextensionsv1alpha1 "github.com/gardener/gardener/pkg/apis/extensions/v1alpha1"
	v1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

func fetchOidcAppsServices(ctx context.Context, c client.Client, object client.Object) (*corev1.ServiceList,
	error) {
	oidcService := &corev1.ServiceList{}
	oidcLabelSelector, _ := labels.Parse(oidc_apps_controller.LabelKey)

	if err := c.List(ctx, oidcService,
		client.InNamespace(object.GetNamespace()),
		client.MatchingLabelsSelector{
			Selector: oidcLabelSelector,
		},
	); err != nil {
		return oidcService, client.IgnoreNotFound(err)
	}

	ownedServices := make([]corev1.Service, 0, len(oidcService.Items))
	for _, service := range oidcService.Items {
		if isAnOwnedResource(object, &service) {
			ownedServices = append(ownedServices, service)
		}
	}

	return &corev1.ServiceList{Items: ownedServices}, nil
}

func fetchOidcAppsIngress(ctx context.Context, c client.Client, object client.Object) (*networkingv1.IngressList,
	error) {
	oidcIngress := &networkingv1.IngressList{}
	oidcLabelSelector, _ := labels.Parse(oidc_apps_controller.LabelKey)

	if err := c.List(ctx, oidcIngress,
		client.InNamespace(object.GetNamespace()),
		client.MatchingLabelsSelector{
			Selector: oidcLabelSelector,
		},
	); err != nil {
		return oidcIngress, client.IgnoreNotFound(err)
	}

	ownedIngresses := make([]networkingv1.Ingress, 0, len(oidcIngress.Items))
	for _, ingress := range oidcIngress.Items {
		if isAnOwnedResource(object, &ingress) {
			ownedIngresses = append(ownedIngresses, ingress)
		}
	}
	return &networkingv1.IngressList{Items: ownedIngresses}, nil
}

func fetchOidcAppsSecrets(ctx context.Context, c client.Client, object client.Object) (*corev1.SecretList,
	error) {
	oidcSecrets := &corev1.SecretList{}
	oidcLabelSelector, _ := labels.Parse(oidc_apps_controller.LabelKey)

	if err := c.List(ctx, oidcSecrets,
		client.InNamespace(object.GetNamespace()),
		client.MatchingLabelsSelector{
			Selector: oidcLabelSelector,
		},
	); err != nil {
		return oidcSecrets, client.IgnoreNotFound(err)
	}

	ownedSecrets := make([]corev1.Secret, 0, len(oidcSecrets.Items))
	for _, secret := range oidcSecrets.Items {
		if isAnOwnedResource(object, &secret) {
			ownedSecrets = append(ownedSecrets, secret)
		}
	}

	return &corev1.SecretList{Items: ownedSecrets}, nil
}

func fetchResourceAttributesNamespace(ctx context.Context, c client.Client, object client.Object) string {
	_log := log.FromContext(ctx)
	// In the case when we are not running on a gardener seed cluster, just return the target namespace
	if os.Getenv(oidc_apps_controller.GARDEN_KUBECONFIG) == "" {
		return object.GetNamespace()
	}
	// In the case the target is in the garden namespace, then we shall not set a namespace.
	// The goal is the kick in only the gardener operators access which should have cluster scoped access
	if object.GetNamespace() == oidc_apps_controller.GARDEN_NAMESPACE {
		return ""
	}
	// In other cases, fetch the cluster resources and set the project namespace
	clusters := &gardenextensionsv1alpha1.ClusterList{}

	if err := c.List(ctx, clusters); err != nil {
		_log.Error(err, "Failed to list Cluster resources")
	}

	for _, cluster := range clusters.Items {
		// Cluster name differ from the target namespace
		if cluster.GetName() != object.GetNamespace() {
			continue
		}
		var shoot gardencorev1beta1.Shoot
		if err := json.Unmarshal(cluster.Spec.Shoot.Raw, &shoot); err != nil {
			_log.Error(err, "Failed to parse the shoot raw extension", "cluster", cluster.Name)
			return ""
		}
		_log.Info("Fetched resource_attribute", "namespace", shoot.GetNamespace(), "shoot", shoot.GetName())
		return shoot.GetNamespace()
	}
	return ""
}

// reconcileDeployementDependencies is the function responsible for managing authentication & authorization dependencies.
// It reconciles the needed secrets, ingresses and services.
func reconcileDeployementDependencies(ctx context.Context, c client.Client, object *v1.Deployment) error {

	// Service for the oauth2-proxy sidecar
	var oauth2Service corev1.Service

	// Ingress for the oauth2-proxy sidecar
	var oauth2Ingress networkingv1.Ingress

	// Secret with oidc configuration for oauth2-proxy sidecar
	var oauth2Secret corev1.Secret

	// Secret with resource attributes for the rbac-proxy sidecar
	var rbacSecret corev1.Secret

	// Secret with oidc CA certificate for the rbac-proxy sidecar
	var oidcCABundleSecret corev1.Secret

	// Optional secret with kubeconfig the rbac-proxy sidecar
	var kubeConfig corev1.Secret

	var (
		mutateFn = func() error { return nil }
		err      error
	)
	if object.GetDeletionTimestamp() == nil {

		if oauth2Secret, err = createOauth2Secret(object); err != nil {
			return fmt.Errorf("failed to create oauth2 secret: %w", err)
		}
		if err = controllerutil.SetOwnerReference(object, &oauth2Secret, c.Scheme()); err != nil {
			return fmt.Errorf("failed to set owner reference to oauth secret: %w", err)
		}
		if _, err = controllerutil.CreateOrUpdate(ctx, c, &oauth2Secret, mutateFn); err != nil {
			return fmt.Errorf("failed to create or update oauth2 secret: %w", err)
		}

		if oauth2Service, err = createOauth2Service(object); err != nil {
			return fmt.Errorf("failed to create oauth2 service: %w", err)
		}
		if err := controllerutil.SetOwnerReference(object, &oauth2Service, c.Scheme()); err != nil {
			return fmt.Errorf("failed to set owner reference to oauth service: %w", err)
		}
		if _, err = controllerutil.CreateOrUpdate(ctx, c, &oauth2Service, mutateFn); err != nil {
			return fmt.Errorf("failed to create or update oauth2 service: %w", err)
		}

		ns := fetchResourceAttributesNamespace(ctx, c, object)
		if rbacSecret, err = createResourceAttributesSecret(object, ns); err != nil {
			return fmt.Errorf("failed to create resource attributes secret: %w", err)
		}
		if err := controllerutil.SetOwnerReference(object, &rbacSecret, c.Scheme()); err != nil {
			return fmt.Errorf("failed to set owner reference to resource attributes secret: %w", err)
		}
		if _, err = controllerutil.CreateOrUpdate(ctx, c, &rbacSecret, mutateFn); err != nil {
			return fmt.Errorf("failed to create or update resource attributes secret secret: %w", err)
		}

		// kubeconfig secret is optionally added to the kube-rbac-proxy
		if kubeConfig, err = createKubeconfigSecret(object); err != nil && !errors.Is(err, errSecretDoesNotExist) {
			return fmt.Errorf("failed to create kubeconfig secret: %w", err)
		}
		if !errors.Is(err, errSecretDoesNotExist) {
			if err = controllerutil.SetOwnerReference(object, &kubeConfig, c.Scheme()); err != nil {
				return fmt.Errorf("failed to set owner reference to kubeconfig secret: %w", err)
			}
			if _, err = controllerutil.CreateOrUpdate(ctx, c, &kubeConfig, mutateFn); err != nil {
				return fmt.Errorf("failed to create or update kubeconfig secret: %w", err)
			}
		}

		// oidc ca bundle secret is mandatory for the rbac-proxy
		if oidcCABundleSecret, err = createOidcCaBundleSecret(object); err != nil && !errors.Is(err, errSecretDoesNotExist) {
			return fmt.Errorf("failed to create oidc ca bundle secret: %w", err)
		}
		if !errors.Is(err, errSecretDoesNotExist) {
			if err = controllerutil.SetOwnerReference(object, &oidcCABundleSecret, c.Scheme()); err != nil {
				return fmt.Errorf("failed to set owner reference to oidc ca bundle secret: %w", err)
			}
			if _, err = controllerutil.CreateOrUpdate(ctx, c, &oidcCABundleSecret, mutateFn); err != nil {
				return fmt.Errorf("failed to create or update oidc ca: %w", err)
			}
		}

		if oauth2Ingress, err = createIngress(object.GetAnnotations()[oidc_apps_controller.AnnotationHostKey], object); err != nil {
			return fmt.Errorf("failed to create oauth2 ingress: %w", err)
		}
		if err = controllerutil.SetOwnerReference(object, &oauth2Ingress,
			c.Scheme()); err != nil {
			return fmt.Errorf("failed to set owner reference to oauth2 ingress: %w", err)
		}
		if _, err = controllerutil.CreateOrUpdate(ctx, c, &oauth2Ingress, mutateFn); err != nil {
			return fmt.Errorf("failed to create or update oauth2 ingress: %w", err)
		}

	}

	return nil
}

func getHash(s string) (string, error) {
	hash := sha256.New()
	if _, err := io.Copy(hash, strings.NewReader(s)); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

func reconcileStatefulSetDependencies(ctx context.Context, c client.Client, object *v1.StatefulSet) error {
	_log := log.FromContext(ctx)

	// Service for the oauth2-proxy sidecar
	var oauth2Service corev1.Service

	// Ingress for the oauth2-proxy sidecar
	var oauth2Ingress networkingv1.Ingress

	// Secret with oidc configuration for oauth2-proxy sidecar
	var oauth2Secret corev1.Secret

	// Secret with resource attributes for the rbac-proxy sidecar
	var rbacSecret corev1.Secret

	// Secret with oidc CA certificate for the rbac-proxy sidecar
	var oidcCABundleSecret corev1.Secret

	// Optional secret with kubeconfig the rbac-proxy sidecar
	var kubeConfig corev1.Secret

	var (
		mutateFn = func() error { return nil }
		err      error
	)
	if object.GetDeletionTimestamp() == nil {

		if oauth2Secret, err = createOauth2Secret(object); err != nil {
			return fmt.Errorf("failed to create oauth2 secret: %w", err)
		}
		if err = controllerutil.SetOwnerReference(object, &oauth2Secret, c.Scheme()); err != nil {
			return fmt.Errorf("failed to set owner reference to oauth secret: %w", err)
		}
		if _, err = controllerutil.CreateOrUpdate(ctx, c, &oauth2Secret, mutateFn); err != nil {
			return fmt.Errorf("failed to create or update oauth2 secret: %w", err)
		}

		// List the Pods
		podList := &corev1.PodList{}
		labelSelector := client.MatchingLabels(object.Spec.Selector.MatchLabels)
		if err := c.List(ctx, podList, labelSelector, client.InNamespace(object.GetNamespace())); err != nil {
			return fmt.Errorf("failed to list pods: %w", err)
		}
		hostPrefix := object.GetAnnotations()[oidc_apps_controller.AnnotationHostKey]
		suffix := object.GetAnnotations()[oidc_apps_controller.AnnotationSuffixKey]
		for _, pod := range podList.Items {
			if len(pod.Annotations) == 0 {
				pod.Annotations = make(map[string]string, 1)
			}
			pod.Annotations[oidc_apps_controller.AnnotationSuffixKey] = suffix

			if oauth2Service, err = createOauth2Service(&pod); err != nil {
				return fmt.Errorf("failed to create oauth2 service: %w", err)
			}
			if err := controllerutil.SetOwnerReference(&pod, &oauth2Service, c.Scheme()); err != nil {
				return fmt.Errorf("failed to set owner reference to oauth service: %w", err)
			}
			if _, err = controllerutil.CreateOrUpdate(ctx, c, &oauth2Service, mutateFn); err != nil {
				return fmt.Errorf("failed to create or update oauth2 service: %w", err)
			}

			// There shall be an ingress for each statefulset pod
			host, domain, found := strings.Cut(hostPrefix, ".")
			if found {
				// In some envorinments, the pod index is added as a label: apps.kubernetes.io/pod-index
				podIndex, present := pod.GetObjectMeta().GetLabels()["statefulset.kubernetes.io/pod-name"]
				if present {
					l := strings.Split(podIndex, "-")
					host = fmt.Sprintf("%s-%s.%s", host, l[len(l)-1], domain)
				} else {
					host = fmt.Sprintf("%s.%s", host, domain)
				}
			}
			_log.V(9).Info("Set", "host", host)
			if oauth2Ingress, err = createIngress(host, &pod); err != nil {
				return fmt.Errorf("failed to create oauth2 ingress: %w", err)
			}
			if err = controllerutil.SetOwnerReference(&pod, &oauth2Ingress, c.Scheme()); err != nil {
				return fmt.Errorf("failed to set owner reference to oauth2 ingress: %w", err)
			}
			if _, err = controllerutil.CreateOrUpdate(ctx, c, &oauth2Ingress, mutateFn); err != nil {
				return fmt.Errorf("failed to create or update oauth2 ingress: %w", err)
			}

		}

		ns := fetchResourceAttributesNamespace(ctx, c, object)
		if rbacSecret, err = createResourceAttributesSecret(object, ns); err != nil {
			return fmt.Errorf("failed to create resource attributes secret: %w", err)
		}
		if err = controllerutil.SetOwnerReference(object, &rbacSecret, c.Scheme()); err != nil {
			return fmt.Errorf("failed to set owner reference to resource attributes secret: %w", err)
		}
		if _, err = controllerutil.CreateOrUpdate(ctx, c, &rbacSecret, mutateFn); err != nil {
			return fmt.Errorf("failed to create or update resource attributes secret: %w", err)
		}

		// kubeconfig secret is optionally added to the kube-rbac-proxy
		if kubeConfig, err = createKubeconfigSecret(object); err != nil && !errors.Is(err, errSecretDoesNotExist) {
			return fmt.Errorf("failed to create kubeconfig secret: %w", err)
		}
		if !errors.Is(err, errSecretDoesNotExist) {
			if err = controllerutil.SetOwnerReference(object, &kubeConfig, c.Scheme()); err != nil {
				return fmt.Errorf("failed to set owner reference to kubeconfig secret: %w", err)
			}
			if _, err = controllerutil.CreateOrUpdate(ctx, c, &kubeConfig, mutateFn); err != nil {
				return fmt.Errorf("failed to create or update kubeconfig secret: %w", err)
			}
		}

		// oidc ca bundle secret is mandatory for the rbac-proxy
		if oidcCABundleSecret, err = createOidcCaBundleSecret(object); err != nil && !errors.Is(err, errSecretDoesNotExist) {
			return fmt.Errorf("failed to create oidc ca bundle secret: %w", err)
		}
		if !errors.Is(err, errSecretDoesNotExist) {
			if err = controllerutil.SetOwnerReference(object, &oidcCABundleSecret, c.Scheme()); err != nil {
				return fmt.Errorf("failed to set owner reference to oidc ca bundle secret: %w", err)
			}
			if _, err = controllerutil.CreateOrUpdate(ctx, c, &oidcCABundleSecret, mutateFn); err != nil {
				return fmt.Errorf("failed to create or update oidc ca: %w", err)
			}
		}

	}

	return nil
}

func triggerGenerationIncrease(ctx context.Context, c client.Client, object client.Object) error {
	gen := object.GetGeneration()
	object.SetGeneration(gen + 1)
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		return c.Update(ctx, object)
	}); err != nil {
		return fmt.Errorf("failed to update object: %w", err)
	}
	return nil
}
