package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"time"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/d2g/dhcp4"
	"github.com/vishvananda/netlink"
)

type PersistedLeased struct {
	ClientID      string
	Ack           *dhcp4.Packet
	LinkName      string
	RenewalTime   time.Time
	RebindingTime time.Time
	ExpireTime    time.Time
	K8sNamespace  string
	K8sPodName    string
	NetNs         string
}

func LoadSavedLeases(leaseFile string, timeout time.Duration, resendMax time.Duration, broadcast bool) ([]*DHCPLease, error) {
	file, err := ioutil.ReadFile(leaseFile)
	if err != nil {
		return nil, err
	}

	var leases []PersistedLeased

	err = json.Unmarshal(file, &leases)

	var reloadedLeases []*DHCPLease

	for _, lease := range leases {
		myLease := &DHCPLease{
			clientID:      lease.ClientID,
			ack:           lease.Ack,
			renewalTime:   lease.RenewalTime,
			rebindingTime: lease.RebindingTime,
			expireTime:    lease.ExpireTime,
			timeout:       timeout,
			resendMax:     resendMax,
			broadcast:     broadcast,
			stop:          make(chan struct{}),
			k8sNamespace:  lease.K8sNamespace,
			k8sPodName:    lease.K8sPodName,
			netNs:         lease.NetNs,
		}
		err := ns.WithNetNSPath(myLease.netNs, func(_ ns.NetNS) error {
			link, err := netlink.LinkByName(lease.LinkName)
			if err != nil {
				return fmt.Errorf("error looking up %q: %v", lease.LinkName, err)
			}

			myLease.link = link

			return nil
		})
		if err != nil {
			if _, ok := err.(ns.NSPathNotExistErr); ok {
				fmt.Printf("Container %s/%s does not seem to have a working netns. Skipping", lease.K8sNamespace, lease.K8sPodName)
				continue
			} else {
				return nil, fmt.Errorf("couldn't look up link '%s' in container netns '%s': %v", lease.LinkName, lease.NetNs, err)
			}
		}
		reloadedLeases = append(reloadedLeases, myLease)
	}

	return reloadedLeases, nil
}

func PersistActiveLeases(fileName string, leases map[string]*DHCPLease) error {
	var leasesToSave []PersistedLeased

	for _, v := range leases {
		value := PersistedLeased{
			ClientID:      v.clientID,
			Ack:           v.ack,
			LinkName:      v.link.Attrs().Name,
			RenewalTime:   v.renewalTime,
			RebindingTime: v.rebindingTime,
			ExpireTime:    v.expireTime,
			K8sNamespace:  v.k8sNamespace,
			K8sPodName:    v.k8sPodName,
			NetNs:         v.netNs,
		}
		leasesToSave = append(leasesToSave, value)
	}

	b, err := json.Marshal(leasesToSave)
	if err != nil {
		return err
	}

	err = ioutil.WriteFile(fileName, b, 0644)
	if err != nil {
		log.Printf("Error while saving: %v", err)
	}
	return nil
}
