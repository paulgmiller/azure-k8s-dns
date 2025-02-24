[Original internal doc](https://msazure.visualstudio.com/CloudNativeCompute/_wiki/wikis/personalplayground/710773/Azure-dns-for-kubernetes)

## Why 
Core dns is great! Coredns was written by people that are probably smarter than me. 
But coredns has to run on customer nodes. Customers can break it. Nodes can go down. Traffic can get segmented and udp load balancing stinks.

AKS scales with cluster but not with usage and we don't throttle when customers blow up coredns. 

Node local caching helps (ALOT) but now we're running yet another service on daemonset on customer nodes costing them resources and us CPU. 


## Can we reuse some azure capabilities

[Azure Private zones](https://learn.microsoft.com/en-us/azure/dns/private-dns-privatednszone)
This thing can today be attached to your vnets will take dns record updates and the azure dns which gets server over imds/wireserver (one of those) answers dns requests directly. AKS doesn't have to worry about it. Up to azure dns team ownthe dns serving problems

They are better at ratelimiting, charging and hopefully debugging that us. 

## Proposal 
* Create a azure private dns zone in the mc group attach it to the cluster vnet. DNS pms say they have a feature to attach to a specific subnet
* The controller in this github will try and full fill the [k8s dns spec](https://github.com/kubernetes/dns/blob/master/docs/specification.md#22---record-for-schema-version) by 
  * watching services and update zone A/AAAA/Srv records
  * watching endpointsslices and updating recordds when their service is headless (cluster ip == none) 
  * Ignoring PTR records for now? (do people actually use this)
* Remove coredns and let normal azure dns server answers.

## Problems
* Since private dns zone attaches to whole vnet only makes sense for managed vnet but Sergio Figuerierdo implied they were ging to let you attach it to a subnet (edns client subnet)?
* Services don't change that often but endpoints do. Would anyone be fool enough to create a 100 pod headless service 
* update time. I did a test and it seemed reasonable (1-2) seconds but need lots more data. SLA is 45 seconds which is worse. Not a big deal for services but again pretty bad if you have a high churn headless service
* https://kubernetes.io/docs/concepts/services-networking/dns-pod-service/#pod-s-dns-policy do these work? 

