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
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	awsresourcev1 "jsbarber.net/vpce/api/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	elbv2 "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/google/uuid"
)

// VPCEReconciler reconciles a VPCE object
type VPCEReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
}

// Finalizer name
const vpceFinalizer = "awsresource.jsbarber.net/finalizer"

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
	log := r.Log.WithValues("vpce", req.NamespacedName)

	if err := r.Get(ctx, req.NamespacedName, &vpce); err != nil {
		if k8serrors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			log.Info("VPCE resource not found. Ignoring since object must be deleted.")
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request.
		log.Error(err, "Failed to get VPCE Object.")
		return ctrl.Result{}, err
	}

	// examine DeletionTimestamp to determine if object is under deletion
	if vpce.ObjectMeta.DeletionTimestamp.IsZero() {
		// The object is not being deleted, so if it does not have our finalizer,
		// then lets add the finalizer and update the object. This is equivalent
		// registering our finalizer.
		if !containsString(vpce.GetFinalizers(), vpceFinalizer) {
			controllerutil.AddFinalizer(&vpce, vpceFinalizer)
			if err := r.Update(ctx, &vpce); err != nil {
				return ctrl.Result{}, err
			}
		}
	} else {
		// The object is being deleted
		if containsString(vpce.GetFinalizers(), vpceFinalizer) {
			// our finalizer is present, so lets handle any external dependency
			if err := r.deleteExternalResources(log, &vpce); err != nil {
				// if fail to delete the external dependency here, return with error
				// so that it can be retried
				return ctrl.Result{}, err
			}

			// remove our finalizer from the list and update it.
			controllerutil.RemoveFinalizer(&vpce, vpceFinalizer)
			if err := r.Update(ctx, &vpce); err != nil {
				return ctrl.Result{}, err
			}
		}

		// Stop reconciliation as the item is being deleted
		return ctrl.Result{}, nil
	}

	// Add finalizer for this CR
	if !controllerutil.ContainsFinalizer(&vpce, vpceFinalizer) {
		controllerutil.AddFinalizer(&vpce, vpceFinalizer)
		err := r.Update(ctx, &vpce)
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	log.Info("Searching for Service: " + vpce.Spec.SvcNamespace + "/" + vpce.Spec.SvcName)
	// r provides an instantiated k8s client - no need to create a new one
	k8sclient := r.Client
	// Get the NLB fqdn
	// TODO use a better way to handle this - eg watch service for updates
	retries := 30
	svcReady := false
	nlbReady := false
	svc := &corev1.Service{}
	// Load service details in svc object
	err := k8sclient.Get(context.Background(), client.ObjectKey{
		Namespace: vpce.Spec.SvcNamespace,
		Name:      vpce.Spec.SvcName,
	}, svc)
	if err != nil {
		log.Error(errors.New("ServiceNotFound"), "Failed to find Service: "+vpce.Spec.SvcName)
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
	// Wait for NLB to be ready
	for i := 1; i < retries; i++ {
		if len(svc.Status.LoadBalancer.Ingress) <= 0 {
			log.Info("Waiting (" + strconv.Itoa(i) + "/" + strconv.Itoa(retries) + " attempts) for Service : " + vpce.Spec.SvcName + " to assign NLB")
		} else {
			svcReady = true
			log.Info("Service : " + vpce.Spec.SvcName + " has assigned NLB")
			break
		}
		time.Sleep(10 * time.Second)
	}
	if !svcReady {
		log.Error(errors.New("ServiceReadyTimeout"), "Service : "+vpce.Spec.SvcName+" has no assigned NLB. Quitting.")
		return ctrl.Result{}, nil
	}

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
	// // Create an Amazon elbv2 service client
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

	// Wait for NLB to become active
	for i := 1; i < retries; i++ {
		//Refresh the state
		nlbDetailsList, err = elbv2client.DescribeLoadBalancers(context.TODO(), params)
		if err != nil {
			log.Error(err, "Failed to Refresh NLB State")
			return ctrl.Result{}, nil
		}
		if nlbDetailsList.LoadBalancers[0].State.Code != "active" {
			log.Info("Waiting (" + strconv.Itoa(i) + "/" + strconv.Itoa(retries) + " attempts) for NLB : " + svc.Status.LoadBalancer.Ingress[0].Hostname + " to become Active. Current state: " + string(nlbDetailsList.LoadBalancers[0].State.Code))
			time.Sleep(5 * time.Second)
		} else {
			log.Info("NLB: " + svc.Status.LoadBalancer.Ingress[0].Hostname + " is now in state: " + string(nlbDetailsList.LoadBalancers[0].State.Code))
			nlbReady = true
			break
		}
	}

	if !nlbReady {
		log.Error(errors.New("NLBReadyTimeout"), "NLB : "+svc.Status.LoadBalancer.Ingress[0].Hostname+" Did not go active in time. Quitting.")
		return ctrl.Result{}, nil
	}

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
		// Update the status
		vpce.Status.NlbArn = nlbARN
		vpce.Status.VpceEndpointServiceId = svcId
		if err := r.Status().Update(ctx, &vpce); err != nil {
			log.Error(err, "unable to update VPCE status")
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	log.Info("No existing VPC Endpoint Service named: " + vpce.Spec.EndpointServiceName + " found. Creating...")

	// Generate a uuid for the client token to ensure idempotency
	clientToken, err := uuid.NewUUID()
	if err != nil {
		log.Error(err, "Error generating ClientToken UUID")
		return ctrl.Result{}, nil
	}
	//Parse the Tags string
	var vpceTags []types.Tag
	for _, tag := range strings.SplitAfter(vpce.Spec.Tags, ",") {
		key := strings.Replace(strings.SplitAfter(tag, "=")[0], ",", "", -1)
		vpceTags = append(vpceTags, types.Tag{
			Key:   aws.String(key),
			Value: aws.String(strings.SplitAfter(tag, "=")[1]),
		})
	}
	createVpceServiceConfigParams := &ec2.CreateVpcEndpointServiceConfigurationInput{
		AcceptanceRequired: &vpce.Spec.AcceptanceRequired,
		ClientToken:        aws.String(clientToken.String()),
		NetworkLoadBalancerArns: []string{
			nlbARN,
		},
	}
	if vpce.Spec.PrivateDnsName != "" {
		createVpceServiceConfigParams.PrivateDnsName = aws.String(vpce.Spec.PrivateDnsName)
	}
	if len(vpceTags) > 0 {
		createVpceServiceConfigParams.TagSpecifications = []types.TagSpecification{
			{
				ResourceType: "vpc-endpoint-service",
				Tags:         vpceTags,
			},
		}
	}

	vpceServiceConfig, err := ec2client.CreateVpcEndpointServiceConfiguration(context.TODO(), createVpceServiceConfigParams)
	if err != nil {
		log.Error(err, "Error creating VPCE Service")
		return ctrl.Result{}, nil
	}
	log.Info("Created new VPCE Service: ID: " + *vpceServiceConfig.ServiceConfiguration.ServiceId + " Name: " + *vpceServiceConfig.ServiceConfiguration.ServiceName)
	// Update the status
	vpce.Status.NlbArn = nlbARN
	vpce.Status.VpceEndpointServiceId = *vpceServiceConfig.ServiceConfiguration.ServiceId
	if err := r.Status().Update(ctx, &vpce); err != nil {
		log.Error(err, "unable to update VPCE status")
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
// TODO add additional resource tyeps to monitor here
func (r *VPCEReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&awsresourcev1.VPCE{}).
		Complete(r)
}

// Finalizer function
func (r *VPCEReconciler) deleteExternalResources(log logr.Logger, vpce *awsresourcev1.VPCE) error {
	// TODO(user): Add the cleanup steps that the operator
	// needs to do before the CR can be deleted. Examples
	// of finalizers include performing backups and deleting
	// resources that are not owned by this CR, like a PVC.
	log.Info("Successfully finalized VPCE: " + vpce.ObjectMeta.Name)
	return nil
}

// Helper functions to check and remove string from a slice of strings.
func containsString(slice []string, s string) bool {
	for _, item := range slice {
		if item == s {
			return true
		}
	}
	return false
}

func removeString(slice []string, s string) (result []string) {
	for _, item := range slice {
		if item == s {
			continue
		}
		result = append(result, item)
	}
	return
}
