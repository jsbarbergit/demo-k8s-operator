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
	"errors"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	awsresourcev1 "jsbarber.net/vpce/api/v1"
	corev1 "k8s.io/api/core/v1"
)

// VPCEReconciler reconciles a VPCE object
type VPCEReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=awsresource.jsbarber.net,resources=vpces,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=awsresource.jsbarber.net,resources=vpces/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=awsresource.jsbarber.net,resources=vpces/finalizers,verbs=update
//+kubebuilder:rbac:groups=v1,resources=service,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the VPCE object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.7.2/pkg/reconcile
func (r *VPCEReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var vpce awsresourcev1.VPCE
	// vpce.Labels
	log := r.Log.WithValues("vpce", req.NamespacedName)

	if err := r.Get(ctx, req.NamespacedName, &vpce); err != nil {
		log.Info("Resource: " + vpce.Name + " removed")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	log.Info("found vpce: " + vpce.Name)
	for key, value := range vpce.Labels {
		log.Info("Labels: " + key + ": " + value)
	}
	log.Info("The Service we're looking for which has NLB details is: " + vpce.Spec.SvcNamespace + "/" + vpce.Spec.SvcName)

	// r provides an instantiated k8s client - no need to create a new one
	k8sclient := r.Client
	// svcs := &corev1.ServiceList{}
	// _ = k8sclient.List(context.Background(), svcs)
	// for _, svc := range svcs.Items {
	// 	log.Info("Found SERVICE: " + svc.Name)
	// 	for key, label := range svc.ObjectMeta.Labels {
	// 		log.Info("Label: " + key + ": " + label)
	// 	}
	// }
	svc := &corev1.Service{}
	err := k8sclient.Get(context.Background(), client.ObjectKey{
		Namespace: vpce.Spec.SvcNamespace,
		Name:      vpce.Spec.SvcName,
	}, svc)
	if err != nil {
		log.Error(err, "Failed to find Service: "+vpce.Spec.SvcName)
		return ctrl.Result{}, nil
	}

	// Validate the Service is the correct type
	if svc.Spec.Type != "LoadBalancer" {
		log.Error(errors.New("InvalidServiceType"), "Service : "+vpce.Spec.SvcName+" is not of type NLB. Got: "+string(svc.Spec.Type))
		return ctrl.Result{}, nil
	}

	// Validate Annotations
	for k, v := range svc.Annotations {
		if k == "service.beta.kubernetes.io/aws-load-balancer-internal" {
			if v != "true" {
				log.Error(errors.New("InvalidServiceAnnotation"), "Service : "+vpce.Spec.SvcName+" does not use an internal NLB. Got: "+v)
				return ctrl.Result{}, nil
			}
		}
		if k == "service.beta.kubernetes.io/aws-load-balancer-type" {
			if v != "nlb" {
				log.Error(errors.New("InvalidServiceAnnotation"), "Service : "+vpce.Spec.SvcName+" does not use an NLB. Got: "+v)
				return ctrl.Result{}, nil
			}
		}
	}

	// Get the NLB fqdn
	// TODO handle resource creation polling for new resources
	nlbFQDN := svc.Status.LoadBalancer.Ingress[0].Hostname
	if len(nlbFQDN) <= 0 {
		log.Error(errors.New("InvalidNLBStatus"), "Service : "+vpce.Spec.SvcName+"'s NLB does not have a Hostname. Got: "+nlbFQDN)
		return ctrl.Result{}, nil
	}
	log.Info("Service: " + vpce.Spec.SvcName + " has an assigned NLB with Hostname: " + nlbFQDN)

	// // Load the Shared AWS Configuration (~/.aws/config)
	// cfg, err := config.LoadDefaultConfig(context.TODO())
	// if err != nil {
	// 	log.Error(err, "Failed to load aws creds")
	// 	return ctrl.Result{}, nil
	// }
	// // Create an Amazon S3 service client
	// client := s3.NewFromConfig(cfg)

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *VPCEReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&awsresourcev1.VPCE{}).
		Complete(r)
}
