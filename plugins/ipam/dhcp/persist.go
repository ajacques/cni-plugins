package main

import (
	"encoding/json"
	"io/ioutil"
	"log"
	"time"

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
}

func LoadSavedLeases(leaseFile string, timeout time.Duration, resendMax time.Duration, broadcast bool,) ([]*DHCPLease, error) {
	file, err := ioutil.ReadFile(leaseFile)
	if err != nil {
		return nil, err
	}

	var leases []PersistedLeased

	err = json.Unmarshal(file, &leases)

	var reloadedLeases []*DHCPLease

	for _, lease := range leases {
		link, err := netlink.LinkByName(lease.LinkName)
		if err != nil {
			return nil, err
		}
		myLease := &DHCPLease{
			clientID: lease.ClientID,
			ack: lease.Ack,
			link: link,
			renewalTime: lease.RenewalTime,
			rebindingTime: lease.RebindingTime,
			expireTime: lease.ExpireTime,
			timeout: timeout,
			resendMax: resendMax,
			broadcast: broadcast,
			stop: make(chan struct{}),
		}
		reloadedLeases = append(reloadedLeases, myLease)
	}

	return reloadedLeases, nil
}

func PersistActiveLeases(fileName string, leases map[string]*DHCPLease) error {
	var leasesToSave []PersistedLeased

	for _, v := range leases {
		value := PersistedLeased{
			ClientID: v.clientID,
			Ack: v.ack,
			LinkName: v.link.Attrs().Name,
			RenewalTime: v.renewalTime,
			RebindingTime: v.rebindingTime,
			ExpireTime: v.expireTime,
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
