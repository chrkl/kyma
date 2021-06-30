/*
Copyright 2021.

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

package controllers

import (
	"context"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	telemetryv1alpha1 "github.com/kyma-project/kyma/components/telemetry-controller/api/v1alpha1"
)

// LoggingConfigurationReconciler reconciles a LoggingConfiguration object
type LoggingConfigurationReconciler struct {
	client.Client
	Scheme             *runtime.Scheme
	FluentBitConfigMap types.NamespacedName
	FluentBitDaemonSet types.NamespacedName
}

//+kubebuilder:rbac:groups=telemetry.kyma-project.io,resources=loggingconfigurations,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=telemetry.kyma-project.io,resources=loggingconfigurations/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=telemetry.kyma-project.io,resources=loggingconfigurations/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=pods,verbs=list;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.8.3/pkg/reconcile
func (r *LoggingConfigurationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	deleted := false
	var config telemetryv1alpha1.LoggingConfiguration
	if err := r.Get(ctx, req.NamespacedName, &config); err != nil {
		log.Info("deleted LoggingConfiguration " + req.Name)
		deleted = true
	}

	var cm corev1.ConfigMap
	if err := r.Get(ctx, r.FluentBitConfigMap, &cm); err != nil {
		log.Info("Fluent Bit ConfigMap not existing, creating new ConfigMap")
		cm = corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      r.FluentBitConfigMap.Name,
				Namespace: r.FluentBitConfigMap.Namespace,
			},
		}
		if err := r.Create(ctx, &cm); err != nil {
			log.Error(err, "Cannot create ConfigMap")
			return ctrl.Result{}, err
		}
	}
	cmKey := req.Name + ".conf"

	if config.DeletionTimestamp != nil || deleted {
		if cm.Data != nil {
			delete(cm.Data, cmKey)
		}
	} else {
		// TODO: Handle Files and Environment
		fluentBitConfig, err := generateFluentBitConfig(config.Spec.Sections)
		if err != nil {
			return ctrl.Result{}, err
		}

		if oldConfig, hasKey := cm.Data[cmKey]; hasKey && oldConfig == fluentBitConfig {
			// Nothing changed
			return ctrl.Result{}, nil
		}

		if cm.Data == nil {
			data := make(map[string]string)
			data[cmKey] = fluentBitConfig
			cm.Data = data
		} else {
			cm.Data[cmKey] = fluentBitConfig
		}
	}

	if err := r.Update(ctx, &cm); err != nil {
		log.Error(err, "cannot update ConfigMap")
		return ctrl.Result{}, err
	}
	log.Info("updated Fluent Bit ConfigMap")

	r.deleteFluentBitPods(ctx, log)

	return ctrl.Result{}, nil
}

func (r *LoggingConfigurationReconciler) deleteFluentBitPods(ctx context.Context, log logr.Logger) error {
	// Restart DaemonSet
	var fluentBitPods corev1.PodList
	podLabels := client.MatchingLabels{
		"app.kubernetes.io/instance": "logging",
		"app.kubernetes.io/name":     "fluent-bit",
	}
	if err := r.List(ctx, &fluentBitPods, client.InNamespace(r.FluentBitDaemonSet.Namespace), podLabels); err != nil {
		log.Error(err, "cannot list fluent bit pods")
		return err
	}
	for _, pod := range fluentBitPods.Items {
		if err := r.Delete(ctx, &pod); err != nil {
			log.Error(err, "cannot delete pod "+pod.Name)
		}
	}
	log.Info("restarting Fluent Bit pods")
	return nil
}

func generateFluentBitConfig(sections []telemetryv1alpha1.Section) (string, error) {
	var result string
	for _, section := range sections {
		result += section.Content + "\n\n"
	}
	return result, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *LoggingConfigurationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&telemetryv1alpha1.LoggingConfiguration{}).
		Owns(&corev1.ConfigMap{}).
		Complete(r)
}
