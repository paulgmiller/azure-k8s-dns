package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	// Core Kubernetes types
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	// Kubebuilder/controller-runtime imports
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	// Azure DNS SDK
	dns "github.com/Azure/azure-sdk-for-go/services/dns/mgmt/2018-05-01/dns"
	"github.com/Azure/go-autorest/autorest/azure/auth"
	"github.com/Azure/go-autorest/autorest/to"
)

// AzureDNSConfig holds Azure-specific configuration for DNS updates.
type AzureDNSConfig struct {
	SubscriptionID string
	ResourceGroup  string
	ZoneName       string // e.g. "myzone.com"
	// If your "ZoneID" is different from the zone name, or you want to store extra references, add them here.
	// ...
}

// Reconciler reconciles changes to Services and Pods
type Reconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	DNSClient     dns.RecordSetsClient
	AzureDNS      AzureDNSConfig
	K8sCoreClient *kubernetes.Clientset
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
		fmt.Println("All flags -subscription, -resourcegroup, -zoneName are required.")
		os.Exit(1)
	}

	// Create an in-cluster config if running in cluster, or fallback to default config.
	// Typically, you'd also allow for out-of-cluster config via e.g. ~/.kube/config
	cfg, err := rest.InClusterConfig()
	if err != nil {
		// Fallback out-of-cluster (dev mode)
		cfg, err = rest.InClusterConfig()
		if err != nil {
			panic(fmt.Sprintf("Unable to get Kubernetes config: %v", err))
		}
	}

	// Create the manager
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: schemeSetup(),
	})
	if err != nil {
		panic(fmt.Sprintf("Unable to start manager: %v", err))
	}

	// Create Azure authorizer
	authorizer, err := auth.NewAuthorizerFromEnvironment()
	if err != nil {
		panic(fmt.Sprintf("failed to create authorizer: %v", err))
	}

	// Initialize the Azure DNS client
	dnsClient := dns.NewRecordSetsClient(*subscriptionID)
	dnsClient.Authorizer = authorizer

	// Create a kubernetes core clientset for additional lookups if needed
	k8sCoreClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		panic(fmt.Sprintf("failed to create k8s core client: %v", err))
	}

	// Initialize the reconciler with the manager client
	r := &Reconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		DNSClient: dnsClient,
		AzureDNS: AzureDNSConfig{
			SubscriptionID: *subscriptionID,
			ResourceGroup:  *resourceGroup,
			ZoneName:       *zoneName,
		},
		K8sCoreClient: k8sCoreClient,
	}

	// Setup watches on Pods and Services
	// Services:
	err = ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Service{}).
		Complete(r)
	if err != nil {
		panic(fmt.Sprintf("Unable to create service controller: %v", err))
	}

	// Pods:
	err = ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Pod{}).
		Complete(r)
	if err != nil {
		panic(fmt.Sprintf("Unable to create pod controller: %v", err))
	}

	// Start the manager
	fmt.Println("Starting manager...")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		panic(fmt.Sprintf("Unable to start manager: %v", err))
	}
}

// Reconcile handles changes to Services or Pods
func (r *Reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	// Requeue interval if we want to re-check things periodically
	requeueInterval := 30 * time.Second
	var svc corev1.Service
	var pod corev1.Pod

	// We don't know if it's a Service or Pod just by the Request, so let's try each.

	// 1) Try to fetch a Service
	err := r.Get(ctx, req.NamespacedName, &svc)
	if err == nil {
		// It's a Service
		if svc.Spec.ClusterIP == "None" {
			// This is a HEADLESS service -> Create/update A/AAAA records for the Endpoints
			// Typically, you'd also watch Endpoints or EndpointSlices; for simplicity, let's just
			// mention the cluster won't route, but you might do a separate watch for endpoints.
			fmt.Printf("Reconciling headless Service %s/%s ...\n", svc.Namespace, svc.Name)
			// For a standard approach: serviceName.namespace + .yourBaseDomain = DNS name
			// e.g. myservice.default.myzone.com
			dnsName := fmt.Sprintf("%s.%s", svc.Name, r.AzureDNS.ZoneName)
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
		}
		// Not headless => do nothing for now
		return reconcile.Result{}, nil
	}

	// 2) Try to fetch a Pod
	err = r.Get(ctx, req.NamespacedName, &pod)
	if err == nil {
		// It's a Pod
		// In some scenarios, you might want each Pod to get a DNS entry like:
		// podName.namespace.yourzone.com
		// or an annotation-based approach: if pod has an annotation `dns-publish=1`, then publish it
		if pod.Annotations["azure-dns-publish"] == "true" {
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

// upsertDNSRecords handles both A (IPv4) and AAAA (IPv6) upserts for a given DNS name
func (r *Reconciler) upsertDNSRecords(ctx context.Context, dnsName string, ipList []string) error {
	// We separate IPv4 vs. IPv6 addresses for the upsert calls.
	var ipv4Addrs []string
	var ipv6Addrs []string
	for _, ip := range ipList {
		if strings.Count(ip, ":") >= 2 {
			ipv6Addrs = append(ipv6Addrs, ip)
		} else {
			ipv4Addrs = append(ipv4Addrs, ip)
		}
	}

	// Upsert A records (if any)
	if len(ipv4Addrs) > 0 {
		if err := r.createOrUpdateARecordSet(ctx, dnsName, ipv4Addrs); err != nil {
			return fmt.Errorf("error upserting A records: %w", err)
		}
	}
	// Upsert AAAA records (if any)
	if len(ipv6Addrs) > 0 {
		if err := r.createOrUpdateAAAARecordSet(ctx, dnsName, ipv6Addrs); err != nil {
			return fmt.Errorf("error upserting AAAA records: %w", err)
		}
	}

	// If there are no addresses, you might want to delete the DNS record or set TTL=0.
	// That is left as an exercise for your environment's preference.

	return nil
}

// createOrUpdateARecordSet wraps the Azure DNS client for an A record.
func (r *Reconciler) createOrUpdateARecordSet(ctx context.Context, dnsName string, ips []string) error {
	// Build ARecords from the IP list
	var aRecords []dns.ARecord
	for _, ip := range ips {
		ipCopy := ip // avoid referencing the loop variable
		aRecords = append(aRecords, dns.ARecord{Ipv4Address: &ipCopy})
	}

	rs := dns.RecordSet{
		RecordSetProperties: &dns.RecordSetProperties{
			TTL:      to.Int64Ptr(300),
			ARecords: &aRecords,
		},
	}

	_, err := r.DNSClient.CreateOrUpdate(
		ctx,
		r.AzureDNS.ResourceGroup,
		r.AzureDNS.ZoneName,
		dnsName, // relative record name or FQDN minus the zone?
		dns.A,
		rs,
		"", // Etag
		"",
	)
	if err != nil {
		return err
	}
	return nil
}

// createOrUpdateAAAARecordSet wraps the Azure DNS client for an AAAA record.
func (r *Reconciler) createOrUpdateAAAARecordSet(ctx context.Context, dnsName string, ips []string) error {
	var aaaaRecords []dns.AaaaRecord
	for _, ip := range ips {
		ipCopy := ip
		aaaaRecords = append(aaaaRecords, dns.AaaaRecord{Ipv6Address: &ipCopy})
	}

	rs := dns.RecordSet{
		RecordSetProperties: &dns.RecordSetProperties{
			TTL:         to.Int64Ptr(300),
			AaaaRecords: &aaaaRecords,
		},
	}

	_, err := r.DNSClient.CreateOrUpdate(
		ctx,
		r.AzureDNS.ResourceGroup,
		r.AzureDNS.ZoneName,
		dnsName,
		dns.AAAA,
		rs,
		"",
		"",
	)
	return err
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
