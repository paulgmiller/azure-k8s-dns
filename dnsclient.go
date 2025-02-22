package main

import (
	"context"
	"fmt"
	"strings"

	dns "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/privatedns/armprivatedns"

	"github.com/Azure/go-autorest/autorest/to"
)

// AzureDNSConfig holds Azure-specific configuration for DNS updates.
type azureDNS struct {
	SubscriptionID string
	ResourceGroup  string
	ZoneName       string // e.g. "myzone.com"
	DNSClient      *dns.RecordSetsClient
	// If your "ZoneID" is different from the zone name, or you want to store extra references, add them here.
	// ...
}

// upsertDNSRecords handles both A (IPv4) and AAAA (IPv6) upserts for a given DNS name
func (r *azureDNS) UpsertDNSRecords(ctx context.Context, dnsName string, ipList []string) error {
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
func (r *azureDNS) createOrUpdateARecordSet(ctx context.Context, dnsName string, ips []string) error {
	// Build ARecords from the IP list
	var aRecords []*dns.ARecord
	for _, ip := range ips {
		ipCopy := ip // avoid referencing the loop variable
		aRecords = append(aRecords, &dns.ARecord{IPv4Address: &ipCopy})
	}

	rs := dns.RecordSet{
		Properties: &dns.RecordSetProperties{
			TTL:      to.Int64Ptr(300),
			ARecords: aRecords,
		},
	}

	_, err := r.DNSClient.CreateOrUpdate(
		ctx,
		r.AzureDNS.ResourceGroup,
		r.AzureDNS.ZoneName,
		dns.RecordTypeA,
		dnsName, // relative record name or FQDN minus the zone?
		rs,
		&dns.RecordSetsClientCreateOrUpdateOptions{},
	)
	if err != nil {
		return err
	}
	return nil
}

// createOrUpdateAAAARecordSet wraps the Azure DNS client for an AAAA record.
func (r *azureDNS) createOrUpdateAAAARecordSet(ctx context.Context, dnsName string, ips []string) error {
	var aaaaRecords []*dns.AaaaRecord
	for _, ip := range ips {
		ipCopy := ip
		aaaaRecords = append(aaaaRecords, &dns.AaaaRecord{IPv6Address: &ipCopy})
	}

	rs := dns.RecordSet{
		Properties: &dns.RecordSetProperties{
			TTL:         to.Int64Ptr(300),
			AaaaRecords: aaaaRecords,
		},
	}

	_, err := r.DNSClient.CreateOrUpdate(
		ctx,
		r.AzureDNS.ResourceGroup,
		r.AzureDNS.ZoneName,
		dns.RecordTypeAAAA,
		dnsName,
		rs,
		&dns.RecordSetsClientCreateOrUpdateOptions{},
	)
	return err
}
