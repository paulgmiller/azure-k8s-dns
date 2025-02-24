package main

import (
	"context"
	"flag"
	"log"

	// Core Kubernetes types
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/rest"

	// Kubebuilder/controller-runtime imports
	ctrl "sigs.k8s.io/controller-runtime"

	// Azure DNS SDK
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/privatedns/armprivatedns"
	dns "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/privatedns/armprivatedns"
)

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

	cfg, err := rest.InClusterConfig()
	if err != nil {
		log.Fatalf("Unable to get Kubernetes config: %v", err)
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

	log.Println("Starting manager...")
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

// schemeSetup sets up the Scheme for corev1 types and any additional CRDs
func schemeSetup() *runtime.Scheme {
	scheme := runtime.NewScheme()
	utilruntime.Must(corev1.AddToScheme(scheme))
	return scheme
}
