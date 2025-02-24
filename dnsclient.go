package main

import (
	"context"
	"fmt"
	"strings"

	dns "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/privatedns/armprivatedns"

	"github.com/Azure/go-autorest/autorest/to"
)

// AzureDNSConfig holds Azure-specific configuration for DNS updates.
type AzureDNSConfig struct {
	SubscriptionID string
	ResourceGroup  string
	ZoneName       string
	DNSClient      *dns.RecordSetsClient
	//TTL?
	//Zone Id?
}

// upsertDNSRecords handles both A (IPv4) and AAAA (IPv6) upserts for a given DNS name
func (r *AzureDNSConfig) UpsertDNSRecords(ctx context.Context, dnsName string, ipList []string) error {
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

func (r *AzureDNSConfig) DeleteDNSRecords(ctx context.Context, dnsName string) error {
	// Delete A records
	if _, err := r.DNSClient.Delete(ctx, r.ResourceGroup, r.ZoneName, dns.RecordTypeA, dnsName, &dns.RecordSetsClientDeleteOptions{}); err != nil {
		return fmt.Errorf("error deleting A records: %w", err)
	}

	// Delete AAAA records
	if _, err := r.DNSClient.Delete(ctx, r.ResourceGroup, r.ZoneName, dns.RecordTypeAAAA, dnsName, &dns.RecordSetsClientDeleteOptions{}); err != nil {
		return fmt.Errorf("error deleting AAAA records: %w", err)
	}

	return nil
}

// createOrUpdateARecordSet wraps the Azure DNS client for an A record.
func (r *AzureDNSConfig) createOrUpdateARecordSet(ctx context.Context, dnsName string, ips []string) error {
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
		r.ResourceGroup,
		r.ZoneName,
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
func (r *AzureDNSConfig) createOrUpdateAAAARecordSet(ctx context.Context, dnsName string, ips []string) error {
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
		r.ResourceGroup,
		r.ZoneName,
		dns.RecordTypeAAAA,
		dnsName,
		rs,
		&dns.RecordSetsClientCreateOrUpdateOptions{},
	)
	return err
}
