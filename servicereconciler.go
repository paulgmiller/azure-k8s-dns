package main

import (
	"context"
	"fmt"
	"log"

	// Core Kubernetes types
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"

	// Kubebuilder/controller-runtime imports
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const finalizer = "dns.azure.com"

type dnsClient interface {
	UpsertDNSRecords(ctx context.Context, dnsName string, ipList []string) error
	DeleteDNSRecords(ctx context.Context, dnsName string) error
}

type ServiceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	dns    dnsClient
}

// Reconcile handles changes to Services or Pods
func (r *ServiceReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	// Requeue interval if we want to re-check things periodically
	var svc corev1.Service
	err := r.Get(ctx, req.NamespacedName, &svc)
	if err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	// Not ding headless services yet.
	if svc.Spec.ClusterIP == "None" {
		// maybe send namespacedname on a channel to endpoint reconciler?
		log.Printf("Ignoring Headless service %s/%s", svc.Namespace, svc.Name)
		return reconcile.Result{}, nil
	}

	dnsName := fmt.Sprintf("%s.%s.svc", svc.Name, svc.Namespace)
	svc = *svc.DeepCopy()
	if svc.DeletionTimestamp != nil {
		if !controllerutil.ContainsFinalizer(&svc, finalizer) {
			return reconcile.Result{}, nil
		}

		log.Printf("Deleting Service %s/%s ...\n", svc.Namespace, svc.Name)
		//send a message to headless to cleanup or do headless ourselves?
		if err := r.dns.DeleteDNSRecords(ctx, dnsName); err != nil {
			return reconcile.Result{}, err
		}
		controllerutil.RemoveFinalizer(&svc, finalizer) //other options instead for finalizers. Perioidic relist and garbage collect
		if err := r.Update(ctx, &svc); err != nil {
			return reconcile.Result{}, err
		}
	}

	log.Printf("Reconciling Service %s/%s ...\n", svc.Namespace, svc.Name)
	// In many real setups, you might prefer <service>.<namespace>.svc.myzone.com or something
	// fully matching your cluster’s DNS. For demonstration, we do a direct subdomain.

	controllerutil.AddFinalizer(&svc, finalizer) //other options instead for finalizers. Perioidic relist and garbage collect
	if err := r.Update(ctx, &svc); err != nil {
		return reconcile.Result{}, err
	}

	// Upsert A/AAAA record sets in Azure
	if err := r.dns.UpsertDNSRecords(ctx, dnsName, svc.Spec.ClusterIPs); err != nil {
		return reconcile.Result{}, err
	}

	log.Printf("Successfully updated DNS for headless Service %s/%s -> %v", svc.Namespace, svc.Name, svc.Spec.ClusterIPs)
	return reconcile.Result{}, nil
}
