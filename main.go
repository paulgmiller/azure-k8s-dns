package main

import (
	"context"
	"flag"
	"fmt"
	"log"

	// Core Kubernetes types
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/rest"

	// Kubebuilder/controller-runtime imports
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	// Azure DNS SDK
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/privatedns/armprivatedns"
	dns "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/privatedns/armprivatedns"
)

type dnsClient interface {
	UpsertDNSRecords(ctx context.Context, dnsName string, ipList []string) error
	DeleteDNSRecords(ctx context.Context, dnsName string) error
}

//https://github.com/kubernetes/dns/blob/master/docs/specification.md
//Goal is to pass as many of these as possible.
//https://github.com/cncf/k8s-conformance/blob/master/docs/KubeConformance-1.32.md#dns-cluster
//https://coredns.io/plugins/kubernetes/
/* Example coredsn kubernetes config in aks. Ignoring pods insecure for now. Going with disabled instead.
   kubernetes cluster.local in-addr.arpa ip6.arpa {
	pods insecure
	fallthrough in-addr.arpa ip6.arpa
	ttl 30
  }*/

//TODO endpoint slices controller for headless
//TODO SRV records
//TODO PTR records

type ServiceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	dns    dnsClient
}

// +kubebuilder:rbac:groups="",resources=pods;services,verbs=get;list;watch

func main() {
	var (
		subscriptionID = flag.String("subscription", "", "Azure subscription ID")
		resourceGroup  = flag.String("resourcegroup", "", "Azure resource group")
		zoneName       = flag.String("zoneName", "cluster.local", "DNS Zone name (e.g. example.com)")
	)
	flag.Parse()
	ctx := context.Background()

	// Basic validation
	if *subscriptionID == "" || *resourceGroup == "" || *zoneName == "" {
		log.Fatal("All flags -subscription, -resourcegroup, -zoneName are required.")
	}

	// Create an in-cluster config if running in cluster, or fallback to default config.
	// Typically, you'd also allow for out-of-cluster config via e.g. ~/.kube/config
	cfg, err := rest.InClusterConfig()
	if err != nil {
		// Fallback out-of-cluster (dev mode)
		cfg, err = rest.InClusterConfig()
		if err != nil {
			log.Fatalf("Unable to get Kubernetes config: %v", err)
		}
	}

	// Create the manager
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: schemeSetup(),
	})
	if err != nil {
		log.Fatalf("Unable to start manager: %v", err)
	}

	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		log.Fatalf("Failed to get Azure credentials: %v", err)
	}
	dnsClient, err := dns.NewRecordSetsClient(*subscriptionID, cred, nil)
	if err != nil {
		log.Fatalf("Failed to get Azure dns client: %v", err)
	}

	// TODO contructor for AzureDNSConfig
	dnscfg := &AzureDNSConfig{
		SubscriptionID: *subscriptionID,
		ResourceGroup:  *resourceGroup,
		ZoneName:       *zoneName,
		DNSClient:      dnsClient,
	}

	MustSetTxTVerion(ctx, dnscfg)

	sr := &ServiceReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		dns:    dnscfg,
	}

	err = ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Service{}).
		//For(&corev1.EndpointSlices{}).
		Complete(sr)
	if err != nil {
		log.Fatalf("Unable to create service controller: %v", err)
	}

	fmt.Println("Starting manager...")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Fatalf("Unable to start manager: %v", err)
	}
}

var specVersion string = "1.1.0"

// move to dnsclient?
func MustSetTxTVerion(ctx context.Context, cfg *AzureDNSConfig) {
	rs := dns.RecordSet{
		Properties: &dns.RecordSetProperties{
			TxtRecords: []*dns.TxtRecord{
				{
					Value: []*string{&specVersion},
				},
			},
		},
	}

	_, err := cfg.DNSClient.CreateOrUpdate(ctx, cfg.ResourceGroup, cfg.ZoneName, armprivatedns.RecordTypeTXT, "dns-version", rs, &armprivatedns.RecordSetsClientCreateOrUpdateOptions{})
	if err != nil {
		log.Fatalf("Failed to update TXT record: %v", err)
	}
}

const finalizer = "dns.azure.com"

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

	if svc.DeletionTimestamp != nil {
		log.Printf("Deleting Service %s/%s ...\n", svc.Namespace, svc.Name)
		//send a message to headless to cleanup or do headless ourselves?
		if err := r.dns.DeleteDNSRecords(ctx, dnsName); err != nil {
			return reconcile.Result{}, err
		}
		//TODO clone service so we don't mutate cache
		controllerutil.RemoveFinalizer(&svc, finalizer) //other options instead for finalizers. Perioidic relist and garbage collect
		if err := r.Update(ctx, &svc); err != nil {
			return reconcile.Result{}, err
		}
	}

	log.Printf("Reconciling Service %s/%s ...\n", svc.Namespace, svc.Name)
	// In many real setups, you might prefer <service>.<namespace>.svc.myzone.com or something
	// fully matching your cluster’s DNS. For demonstration, we do a direct subdomain.

	//TODO clone service so we don't mutate cache
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

// schemeSetup sets up the Scheme for corev1 types and any additional CRDs
func schemeSetup() *runtime.Scheme {
	scheme := runtime.NewScheme()
	utilruntime.Must(corev1.AddToScheme(scheme))

	// If you had custom resources, you'd add them here:
	// utilruntime.Must(mycrdv1.AddToScheme(scheme))

	// For older cluster-runtime usage, you might also do:
	// scheme.AddKnownTypes(schema.GroupVersion{Group: "", Version: "v1"}, &corev1.Service{}, &corev1.Pod{})
	// ...
	return scheme
}
