package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"strings"

	// Core Kubernetes types
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/rest"

	// Kubebuilder/controller-runtime imports
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	// Azure DNS SDK
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	dns "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/privatedns/armprivatedns"
	"github.com/Azure/go-autorest/autorest/to"
)



type dnsClient interface {
	UpsertDNSRecords(ctx context.Context, dnsName string, ipList []string) error 
}


// Reconciler reconciles changes to Services and Pods
type ServiceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	dns dnsClient 

}

//endpoints reconciler. 


type PodReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	dns dnsClient 
}

// +kubebuilder:rbac:groups="",resources=pods;services,verbs=get;list;watch

func main() {
	var (
		subscriptionID = flag.String("subscription", "", "Azure subscription ID")
		resourceGroup  = flag.String("resourcegroup", "", "Azure resource group")
		zoneName       = flag.String("zoneName", "", "DNS Zone name (e.g. example.com)")
	)
	flag.Parse()

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

	cfg := AzureDNSConfig{
		SubscriptionID: *subscriptionID,
		ResourceGroup:  *resourceGroup,
		ZoneName:       *zoneName,
		dnsClient:      dnsClient,
	}
	// Initialize the reconciler with the manager client
	pr := &PodReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		dns: cfg,
	}

	sr := &ServiceReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		dns: cfg,
	}

	// Setup watches on Pods and Services
	// Services:
	err = ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Service{}).
		Complete(sr)
	if err != nil {
		log.Fatalf("Unable to create service controller: %v", err)
	}

	// Pods:
	err = ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Pod{}).
		Complete(pr)
	if err != nil {
		log.Fatalf("Unable to create pod controller: %v", err)
	}

	// Start the manager
	fmt.Println("Starting manager...")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Fatalf("Unable to start manager: %v", err)
	}
}

// Reconcile handles changes to Services or Pods
func (r *ServiceReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	// Requeue interval if we want to re-check things periodically
	var svc corev1.Service

	// We don't know if it's a Service or Pod just by the Request, so let's try each.

	// 1) Try to fetch a Service
	err := r.Get(ctx, req.NamespacedName, &svc)
	if err != nil {
		//todo finalizer?
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	

		// It's a Service
		if svc.Spec.ClusterIP == "None" {
			// This is a HEADLESS service -> Create/update A/AAAA records for the Endpoints
			// Typically, you'd also watch Endpoints or EndpointSlices; for simplicity, let's just
			// mention the cluster won't route, but you might do a separate watch for endpoints.
			fmt.Printf("Reconciling headless Service %s/%s ...\n", svc.Namespace, svc.Name)
			// For a standard approach: serviceName.namespace + .yourBaseDomain = DNS name
			// e.g. myservice.default.myzone.com
			dnsName := fmt.Sprintf("%s.%s.svc", svc.Name, r.AzureDNS.ZoneName)
			// In many real setups, you might prefer <service>.<namespace>.svc.myzone.com or something
			// fully matching your cluster’s DNS. For demonstration, we do a direct subdomain.
			ipList, err := r.getPodIPsForHeadlessService(ctx, &svc)
			if err != nil {
				fmt.Printf("Error listing pods for headless service: %v\n", err)
				// We'll requeue to try again
				return reconcile.Result{RequeueAfter: requeueInterval}, nil
			}
			// Upsert A/AAAA record sets in Azure
			if err := r.upsertDNSRecords(ctx, dnsName, ipList); err != nil {
				fmt.Printf("Failed to upsert DNS records: %v\n", err)
			} else {
				fmt.Printf("Successfully updated DNS for headless Service %s/%s -> %v\n",
					svc.Namespace, svc.Name, ipList)
			}
		
		// Not headless => do nothing for now
		return reconcile.Result{}, nil
	}
}

func (r *PodReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {

	// 2) Try to fetch a Pod
	var pod corev1.Pod
	err = r.Get(ctx, req.NamespacedName, &pod)
	if err == nil {
		// It's a Pod
		// In some scenarios, you might want each Pod to get a DNS entry like:
		// podName.namespace.yourzone.com
		// or an annotation-based approach: if pod has an annotation `dns-publish=1`, then publish it
			fmt.Printf("Reconciling Pod %s/%s for DNS publishing...\n", pod.Namespace, pod.Name)
			dnsName := fmt.Sprintf("%s.%s.%s", pod.Name, pod.Namespace, r.AzureDNS.ZoneName)
			// Possibly handle multiple IP addresses, e.g. for dual-stack
			ipList := []string{}
			if pod.Status.PodIP != "" {
				ipList = append(ipList, pod.Status.PodIP)
			}
			// Upsert A/AAAA record sets in Azure
			if err := r.upsertDNSRecords(ctx, dnsName, ipList); err != nil {
				fmt.Printf("Failed to upsert DNS records for pod: %v\n", err)
			} else {
				fmt.Printf("Successfully updated DNS for Pod %s/%s -> %v\n",
					pod.Namespace, pod.Name, ipList)
			}
		}
		return reconcile.Result{}, nil
	}

	// If neither a Service nor a Pod => just ignore
	return reconcile.Result{}, nil
}





// getPodIPsForHeadlessService queries Pods that match the Service’s selector
func (r *Reconciler) getPodIPsForHeadlessService(ctx context.Context, svc *corev1.Service) ([]string, error) {
	// If Service has a selector, we can search for Pods by label. If no selector, might be manual Endpoints.
	selector := svc.Spec.Selector
	if len(selector) == 0 {
		return nil, fmt.Errorf("headless service has no selector, skipping")
	}
	// Build a label selector from the service’s .spec.selector
	labelSelector := client.MatchingLabels(selector)
	var podList corev1.PodList
	err := r.List(ctx, &podList, client.InNamespace(svc.Namespace), labelSelector)
	if err != nil {
		return nil, err
	}

	var ipList []string
	for _, p := range podList.Items {
		if p.Status.PodIP != "" {
			ipList = append(ipList, p.Status.PodIP)
		}
	}
	return ipList, nil
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
