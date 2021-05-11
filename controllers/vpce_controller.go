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
	"strings"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	awsresourcev1 "jsbarber.net/vpce/api/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	elbv2 "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	"github.com/aws/aws-sdk-go/aws"
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
		log.Info("Resource removed")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	log.Info("Managing existing VPCE Resource: " + vpce.Name)
	log.Info("Searching for Service: " + vpce.Spec.SvcNamespace + "/" + vpce.Spec.SvcName)
	// r provides an instantiated k8s client - no need to create a new one
	k8sclient := r.Client

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

	// Extract the NLB resource name, which is the hostname part of the FQDN
	// FQDN should be in the form: <UniqueHostname-UUID>.elb.<REGION>.amazonaws.com
	_nlbHostnameParts := strings.SplitAfter(nlbFQDN, "-")
	if len(_nlbHostnameParts) < 2 {
		log.Error(errors.New("InvalidNLBFQDN"), "Failed to parse NLB FQDN. Got: "+nlbFQDN)
		return ctrl.Result{}, nil
	}
	nlbHostname := strings.Replace(_nlbHostnameParts[0], "-", "", -1)
	log.Info("Fetching ARN for NLB: " + nlbHostname)

	// Load the Shared AWS Configuration (~/.aws/config)
	// TODO detect and use IRSA token when running in K8S
	cfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		log.Error(err, "Failed to load aws creds")
		return ctrl.Result{}, nil
	}
	// // Create an Amazon S3 service client
	elbv2client := elbv2.NewFromConfig(cfg)
	params := &elbv2.DescribeLoadBalancersInput{
		Names: []string{
			nlbHostname,
		},
	}
	nlbDetailsList, err := elbv2client.DescribeLoadBalancers(context.TODO(), params)
	if err != nil {
		log.Error(err, "Failed to describe nlb")
		return ctrl.Result{}, nil
	}

	nlbARN := *aws.String(*nlbDetailsList.LoadBalancers[0].LoadBalancerArn)
	log.Info("Got NLB ARN: " + nlbARN)

	// Check for existing VPCE Service Config
	ec2client := ec2.NewFromConfig(cfg)
	vpceServiceConfigParams := &ec2.DescribeVpcEndpointServiceConfigurationsInput{
		Filters: []types.Filter{
			{
				Name:   aws.String("tag:Name"),
				Values: []string{vpce.Spec.EndpointServiceName},
			},
		},
	}

	vpceServiceConfigs, err := ec2client.DescribeVpcEndpointServiceConfigurations(context.TODO(), vpceServiceConfigParams)
	if len(vpceServiceConfigs.ServiceConfigurations) > 0 {
		svcId := *aws.String(*vpceServiceConfigs.ServiceConfigurations[0].ServiceId)
		svcStatus := *aws.String(string(vpceServiceConfigs.ServiceConfigurations[0].ServiceState))
		log.Info("Existing VPC Service Endpoint with matching Name found: Name: " + vpce.Spec.EndpointServiceName + ", ID: " + svcId + ", Status: " + svcStatus + ". No Action Needed")
		return ctrl.Result{}, nil
	}

	// for _, v := range vpceServiceConfigs.ServiceConfigurations {
	// 	println("got v: " + *aws.String(*v.ServiceId))
	// }

	log.Info("No existing VPC Endpoint Service named: " + vpce.Spec.EndpointServiceName + " found. Creating...")
	// TODO Add the code to create the Endpoint Service here

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *VPCEReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&awsresourcev1.VPCE{}).
		Complete(r)
}
