// Copyright 2014 CNI authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"regexp"
	"runtime"
	"sort"
	"syscall"
	"time"

	"github.com/coreos/go-iptables/iptables"
	"github.com/vishvananda/netlink"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/cni/pkg/version"
	"github.com/containernetworking/plugins/pkg/ip"
	"github.com/containernetworking/plugins/pkg/ipam"
	"github.com/containernetworking/plugins/pkg/link"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/containernetworking/plugins/pkg/utils"
	bv "github.com/containernetworking/plugins/pkg/utils/buildversion"
	"github.com/containernetworking/plugins/pkg/utils/sysctl"
)

// For testcases to force an error after IPAM has been performed
var debugPostIPAMError error

const defaultBrName = "cni0"

type NetConf struct {
	types.NetConf
	BrName          string `json:"bridge"`
	IsGW            bool   `json:"isGateway"`
	IsDefaultGW     bool   `json:"isDefaultGateway"`
	ForceAddress    bool   `json:"forceAddress"`
	IPMasq          bool   `json:"ipMasq"`
	MTU             int    `json:"mtu"`
	HairpinMode     bool   `json:"hairpinMode"`
	PromiscMode     bool   `json:"promiscMode"`
	Vlan            int    `json:"vlan"`
	MacSpoofChk     bool   `json:"macspoofchk,omitempty"`
	EnableDad       bool   `json:"enabledad,omitempty"`
	UplinkInterface string `json:"uplinkInterface"`
	EnableIPv6      bool   `json:"enableIPv6"`

	Args struct {
		Cni BridgeArgs `json:"cni,omitempty"`
	} `json:"args,omitempty"`
	RuntimeConfig struct {
		Mac string `json:"mac,omitempty"`
	} `json:"runtimeConfig,omitempty"`

	mac string
}

type BridgeArgs struct {
	Mac string `json:"mac,omitempty"`
}

// MacEnvArgs represents CNI_ARGS
type MacEnvArgs struct {
	types.CommonArgs
	MAC types.UnmarshallableString `json:"mac,omitempty"`
}

type gwInfo struct {
	gws               []net.IPNet
	family            int
	defaultRouteFound bool
}

func init() {
	// this ensures that main runs only on main thread (thread group leader).
	// since namespace ops (unshare, setns) are done for a single thread, we
	// must ensure that the goroutine does not jump from OS thread to thread
	runtime.LockOSThread()
}

func loadNetConf(bytes []byte, envArgs string) (*NetConf, string, error) {
	n := &NetConf{
		BrName: defaultBrName,
	}
	if err := json.Unmarshal(bytes, n); err != nil {
		return nil, "", fmt.Errorf("failed to load netconf: %v", err)
	}
	if n.Vlan < 0 || n.Vlan > 4094 {
		return nil, "", fmt.Errorf("invalid VLAN ID %d (must be between 0 and 4094)", n.Vlan)
	}

	if envArgs != "" {
		e := MacEnvArgs{}
		if err := types.LoadArgs(envArgs, &e); err != nil {
			return nil, "", err
		}

		if e.MAC != "" {
			n.mac = string(e.MAC)
		}
	}

	if mac := n.Args.Cni.Mac; mac != "" {
		n.mac = mac
	}

	if mac := n.RuntimeConfig.Mac; mac != "" {
		n.mac = mac
	}

	return n, n.CNIVersion, nil
}

// calcGateways processes the results from the IPAM plugin and does the
// following for each IP family:
//    - Calculates and compiles a list of gateway addresses
//    - Adds a default route if needed
func calcGateways(result *current.Result, n *NetConf) (*gwInfo, *gwInfo, error) {

	gwsV4 := &gwInfo{}
	gwsV6 := &gwInfo{}

	for _, ipc := range result.IPs {

		// Determine if this config is IPv4 or IPv6
		var gws *gwInfo
		defaultNet := &net.IPNet{}
		switch {
		case ipc.Address.IP.To4() != nil:
			gws = gwsV4
			gws.family = netlink.FAMILY_V4
			defaultNet.IP = net.IPv4zero
		case len(ipc.Address.IP) == net.IPv6len:
			gws = gwsV6
			gws.family = netlink.FAMILY_V6
			defaultNet.IP = net.IPv6zero
		default:
			return nil, nil, fmt.Errorf("Unknown IP object: %v", ipc)
		}
		defaultNet.Mask = net.IPMask(defaultNet.IP)

		// All IPs currently refer to the container interface
		ipc.Interface = current.Int(2)

		// If not provided, calculate the gateway address corresponding
		// to the selected IP address
		if ipc.Gateway == nil && n.IsGW {
			ipc.Gateway = calcGatewayIP(&ipc.Address)
		}

		// Add a default route for this family using the current
		// gateway address if necessary.
		if n.IsDefaultGW && !gws.defaultRouteFound {
			for _, route := range result.Routes {
				if route.GW != nil && defaultNet.String() == route.Dst.String() {
					gws.defaultRouteFound = true
					break
				}
			}
			if !gws.defaultRouteFound {
				result.Routes = append(
					result.Routes,
					&types.Route{Dst: *defaultNet, GW: ipc.Gateway},
				)
				gws.defaultRouteFound = true
			}
		}

		// Append this gateway address to the list of gateways
		if n.IsGW {
			gw := net.IPNet{
				IP:   ipc.Gateway,
				Mask: ipc.Address.Mask,
			}
			gws.gws = append(gws.gws, gw)
		}
	}
	return gwsV4, gwsV6, nil
}

func ensureAddr(br netlink.Link, family int, ipn *net.IPNet, forceAddress bool) error {
	addrs, err := netlink.AddrList(br, family)
	if err != nil && err != syscall.ENOENT {
		return fmt.Errorf("could not get list of IP addresses: %v", err)
	}

	ipnStr := ipn.String()
	for _, a := range addrs {

		// string comp is actually easiest for doing IPNet comps
		if a.IPNet.String() == ipnStr {
			return nil
		}

		// Multiple IPv6 addresses are allowed on the bridge if the
		// corresponding subnets do not overlap. For IPv4 or for
		// overlapping IPv6 subnets, reconfigure the IP address if
		// forceAddress is true, otherwise throw an error.
		if family == netlink.FAMILY_V4 || a.IPNet.Contains(ipn.IP) || ipn.Contains(a.IPNet.IP) {
			if forceAddress {
				if err = deleteAddr(br, a.IPNet); err != nil {
					return err
				}
			} else {
				return fmt.Errorf("%q already has an IP address different from %v", br.Attrs().Name, ipnStr)
			}
		}
	}

	addr := &netlink.Addr{IPNet: ipn, Label: ""}
	if err := netlink.AddrAdd(br, addr); err != nil && err != syscall.EEXIST {
		return fmt.Errorf("could not add IP address to %q: %v", br.Attrs().Name, err)
	}

	// Set the bridge's MAC to itself. Otherwise, the bridge will take the
	// lowest-numbered mac on the bridge, and will change as ifs churn
	if err := netlink.LinkSetHardwareAddr(br, br.Attrs().HardwareAddr); err != nil {
		return fmt.Errorf("could not set bridge's mac: %v", err)
	}

	return nil
}

func deleteAddr(br netlink.Link, ipn *net.IPNet) error {
	addr := &netlink.Addr{IPNet: ipn, Label: ""}

	if err := netlink.AddrDel(br, addr); err != nil {
		return fmt.Errorf("could not remove IP address from %q: %v", br.Attrs().Name, err)
	}

	return nil
}

func bridgeByName(name string) (*netlink.Bridge, error) {
	l, err := netlink.LinkByName(name)
	if err != nil {
		return nil, fmt.Errorf("could not lookup %q: %v", name, err)
	}
	br, ok := l.(*netlink.Bridge)
	if !ok {
		return nil, fmt.Errorf("%q already exists but is not a bridge", name)
	}
	return br, nil
}

func copyAddress(from netlink.Link, to netlink.Link, family int) (bool, *netlink.Addr, error) {
	uplinkAddrs, err := netlink.AddrList(from, family)
	if err != nil {
		return false, nil, fmt.Errorf("couldn't find IPv4 addresses for ")
	}

	addrs, err := netlink.AddrList(to, family)
	if err != nil {
		return false, nil, fmt.Errorf("couldn't get addrs for interface '%s': %v", from.Attrs().Name, err)
	}
	if len(uplinkAddrs) == 0 {
		if len(addrs) > 0 {
			// Bridge already has the IP address
			return false, &addrs[0], nil
		}
		return false, nil, fmt.Errorf("didn't find any IP addresses for interface '%s'", from.Attrs().Name)
	}
	oldAddr := uplinkAddrs[0]
	foundAddr := false
	for _, addr := range addrs {
		if addr.Equal(oldAddr) {
			foundAddr = true
			break
		}
	}
	newAddr := netlink.Addr{
		IPNet:       oldAddr.IPNet,
		Scope:       oldAddr.Scope,
		PreferedLft: oldAddr.PreferedLft,
		ValidLft:    oldAddr.ValidLft,
	}
	if !foundAddr {
		err = netlink.AddrAdd(to, &newAddr)
		if err != nil {
			return false, nil, fmt.Errorf("couldn't add IP address '%s' to interface '%s': %v", newAddr.IP, to.Attrs().Name, err)
		}
	}
	return !foundAddr, &newAddr, nil
}

func findMatchingInterface(ifaceName string) (netlink.Link, error) {
	links, err := netlink.LinkList()
	if err != nil {
		return nil, fmt.Errorf("failed to list interfaces: %v", err)
	}
	r, err := regexp.Compile(ifaceName)
	if err != nil {
		return nil, fmt.Errorf("invalid uplink interface regex: %v", err)
	}

	set := ""

	for _, l := range links {
		if r.MatchString(l.Attrs().Name) {
			return l, nil
		}
		set = l.Attrs().Name + "," + set
	}

	return nil, fmt.Errorf("couldn't find any matching interfaces '%s' (%s) in set: %s", ifaceName, r, set)
}

func ensureBridge(brName string, mtu int, promiscMode, vlanFiltering bool, uplinkLink netlink.Link, enableIPv6 bool) (*netlink.Bridge, error) {
	br := &netlink.Bridge{
		LinkAttrs: netlink.LinkAttrs{
			Name: brName,
			MTU:  mtu,
			// Let kernel use default txqueuelen; leaving it unset
			// means 0, and a zero-length TX queue messes up FIFO
			// traffic shapers which use TX queue length as the
			// default packet limit
			TxQLen: -1,
		},
	}
	if vlanFiltering {
		br.VlanFiltering = &vlanFiltering
	}

	err := netlink.LinkAdd(br)
	if err != nil && err != syscall.EEXIST {
		return nil, fmt.Errorf("could not add %q: %v", brName, err)
	}

	if promiscMode {
		if err := netlink.SetPromiscOn(br); err != nil {
			return nil, fmt.Errorf("could not set promiscuous mode on %q: %v", brName, err)
		}
	}

	// Re-fetch link to read all attributes and if it already existed,
	// ensure it's really a bridge with similar configuration
	br, err = bridgeByName(brName)
	if err != nil {
		return nil, err
	}

	// we want to own the routes for this interface
	if enableIPv6 {
		_, _ = sysctl.Sysctl(fmt.Sprintf("net/ipv6/conf/%s/accept_ra", brName), "1")

		_, err = sysctl.Sysctl(fmt.Sprintf("net/ipv6/conf/%s/forwarding", brName), "1")
		if err != nil {
			return nil, fmt.Errorf("could not enable IPv6 routing on '%s': %v", brName, err)
		}
	}

	if err := netlink.LinkSetUp(br); err != nil {
		return nil, err
	}

	uplinkName := uplinkLink.Attrs().Name

	var failed bool
	applied, gwIp, err := copyAddress(uplinkLink, br, netlink.FAMILY_V4)
	if err != nil {
		return nil, fmt.Errorf("couldn't copy IPv4 address to bridge: %v", err)
	}
	if applied {
		defer func() {
			if failed {
				netlink.AddrDel(br, gwIp)
			}
		}()
	}

	// Add the uplink interface to the bridge if it isn't already there
	if uplinkLink.Attrs().MasterIndex != br.Attrs().Index && uplinkLink.Attrs().MasterIndex != 0 {
		master, err := netlink.LinkByIndex(uplinkLink.Attrs().MasterIndex)
		if err != nil {
			failed = true
			return nil, fmt.Errorf("interface %s has already a master set (actual=%d, desired=%d), could not retrieve the name: %v", uplinkName, uplinkLink.Attrs().MasterIndex, br.Attrs().Index, err)
		}
		return nil, fmt.Errorf("interface %s has already a master set: %s", uplinkName, master.Attrs().Name)
	}

	// https://backreference.org/2010/07/28/linux-bridge-mac-addresses-and-dynamic-ports/
	err = netlink.LinkSetHardwareAddr(br, uplinkLink.Attrs().HardwareAddr)
	if err != nil {
		failed = true
		return nil, fmt.Errorf("couldn't assign bridge MAC address to the same as the uplink interface: %v", err)
	}

	err = netlink.LinkSetMaster(uplinkLink, br)
	if err != nil {
		failed = true
		return nil, fmt.Errorf("couldn't add interface '%s' to bridge '%s': %v", uplinkName, brName, err)
	}
	// Routes on the uplink (e.g. eth0) interface need to be moved to the bridge so the kernel correctly routes packets
	routes, err := netlink.RouteList(uplinkLink, netlink.FAMILY_V4)
	if err != nil {
		failed = true
		return nil, fmt.Errorf("couldn't get routes for uplink interface to move to bridge: %v", err)
	}
	if len(routes) > 0 {
		// Sort routes so that most specific routes appear first. This is to avoid an issue where we can't create a
		// default route until the subnet route is available
		sort.Slice(routes, func(i, j int) bool {
			l, _ := routes[i].Dst.Mask.Size()
			if routes[j].Dst == nil {
				return true
			}
			if routes[j].Dst.Mask == nil {
				return true
			}
			r, _ := routes[j].Dst.Mask.Size()
			return l >= r
		})
		for _, route := range routes {
			err = netlink.RouteDel(&route)
			if err != nil {
				failed = true
				return nil, fmt.Errorf("couldn't delete route from uplink: %v", err)
			}
			route.LinkIndex = br.Index
			err = netlink.RouteAdd(&route)
			if err != nil {
				failed = true
				return nil, fmt.Errorf("couldn't move route to bridge: %v", err)
			}
		}
	}

	return br, nil
}

func ensureVlanInterface(br *netlink.Bridge, vlanId int) (netlink.Link, error) {
	name := fmt.Sprintf("%s.%d", br.Name, vlanId)

	brGatewayVeth, err := netlink.LinkByName(name)
	if err != nil {
		if err.Error() != "Link not found" {
			return nil, fmt.Errorf("failed to find interface %q: %v", name, err)
		}

		hostNS, err := ns.GetCurrentNS()
		if err != nil {
			return nil, fmt.Errorf("faild to find host namespace: %v", err)
		}

		_, brGatewayIface, err := setupVeth(hostNS, br, name, br.MTU, false, vlanId, "")
		if err != nil {
			return nil, fmt.Errorf("faild to create vlan gateway %q: %v", name, err)
		}

		brGatewayVeth, err = netlink.LinkByName(brGatewayIface.Name)
		if err != nil {
			return nil, fmt.Errorf("failed to lookup %q: %v", brGatewayIface.Name, err)
		}

		err = netlink.LinkSetUp(brGatewayVeth)
		if err != nil {
			return nil, fmt.Errorf("failed to up %q: %v", brGatewayIface.Name, err)
		}
	}

	return brGatewayVeth, nil
}

func setupVeth(netns ns.NetNS, br *netlink.Bridge, ifName string, mtu int, hairpinMode bool, vlanID int, mac string) (*current.Interface, *current.Interface, error) {
	contIface := &current.Interface{}
	hostIface := &current.Interface{}

	err := netns.Do(func(hostNS ns.NetNS) error {
		// create the veth pair in the container and move host end into host netns
		hostVeth, containerVeth, err := ip.SetupVeth(ifName, mtu, mac, hostNS)
		if err != nil {
			return err
		}
		contIface.Name = containerVeth.Name
		contIface.Mac = containerVeth.HardwareAddr.String()
		contIface.Sandbox = netns.Path()
		hostIface.Name = hostVeth.Name
		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	// need to lookup hostVeth again as its index has changed during ns move
	hostVeth, err := netlink.LinkByName(hostIface.Name)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to lookup %q: %v", hostIface.Name, err)
	}
	hostIface.Mac = hostVeth.Attrs().HardwareAddr.String()

	// connect host veth end to the bridge
	if err := netlink.LinkSetMaster(hostVeth, br); err != nil {
		return nil, nil, fmt.Errorf("failed to connect %q to bridge %v: %v", hostVeth.Attrs().Name, br.Attrs().Name, err)
	}

	// set hairpin mode
	if err = netlink.LinkSetHairpin(hostVeth, hairpinMode); err != nil {
		return nil, nil, fmt.Errorf("failed to setup hairpin mode for %v: %v", hostVeth.Attrs().Name, err)
	}

	if vlanID != 0 {
		err = netlink.BridgeVlanAdd(hostVeth, uint16(vlanID), true, true, false, true)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to setup vlan tag on interface %q: %v", hostIface.Name, err)
		}
	}

	return hostIface, contIface, nil
}

func calcGatewayIP(ipn *net.IPNet) net.IP {
	nid := ipn.IP.Mask(ipn.Mask)
	return ip.NextIP(nid)
}

func setupBridge(n *NetConf) (*netlink.Bridge, *current.Interface, error) {
	vlanFiltering := false
	if n.Vlan != 0 {
		vlanFiltering = true
	}

	uplinkIface, err := findMatchingInterface(n.UplinkInterface)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to find uplink interface matching regex %q: %v", n.UplinkInterface, err)
	}

	// create bridge if necessary
	br, err := ensureBridge(n.BrName, n.MTU, n.PromiscMode, vlanFiltering, uplinkIface, n.EnableIPv6)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create bridge %q: %v", n.BrName, err)
	}

	return br, &current.Interface{
		Name: br.Attrs().Name,
		Mac:  br.Attrs().HardwareAddr.String(),
	}, nil
}

func enableIPForward(family int) error {
	if family == netlink.FAMILY_V4 {
		return ip.EnableIP4Forward()
	}
	return ip.EnableIP6Forward()
}

func createBaselineRules(brName string) [][]string {
	rules := make([][]string, 0)

	// TODO: Use marking to track exactly which interface

	rules = append(rules, []string{"-i", "cni0", "-j", "ACCEPT"})

	return rules
}

func setupFirewallRules(ipt *iptables.IPTables, vethName string) error {
	rules := make([][]string, 0)
	err := utils.EnsureChain(ipt, "filter", "CNI-FORWARD")
	if err != nil {
		return fmt.Errorf("failed to create chain: %v", err)
	}

	err = utils.EnsureFirstChainRule(ipt, "FORWARD", utils.GenerateFilterRule("CNI-FORWARD"))
	if err != nil {
		return err
	}

	rules = append(rules, createBaselineRules(vethName)...)

	for _, rule := range rules {
		err = ipt.AppendUnique("filter", "CNI-FORWARD", rule...)
		if err != nil {
			return err
		}
	}

	return nil
}

func cleanupRules(ipt *iptables.IPTables, rules [][]string) {
	for _, rule := range rules {
		ipt.Delete("filter", "CNI-FORWARD", rule...)
	}
}

func cmdAdd(args *skel.CmdArgs) error {
	var success bool = false

	n, cniVersion, err := loadNetConf(args.StdinData, args.Args)
	if err != nil {
		return err
	}

	isLayer3 := n.IPAM.Type != ""

	if n.IsDefaultGW {
		n.IsGW = true
	}

	if n.HairpinMode && n.PromiscMode {
		return fmt.Errorf("cannot set hairpin mode and promiscuous mode at the same time.")
	}

	br, brInterface, err := setupBridge(n)
	if err != nil {
		return err
	}

	netns, err := ns.GetNS(args.Netns)
	if err != nil {
		return fmt.Errorf("failed to open netns %q: %v", args.Netns, err)
	}
	defer netns.Close()

	hostInterface, containerInterface, err := setupVeth(netns, br, args.IfName, n.MTU, n.HairpinMode, n.Vlan, n.mac)
	if err != nil {
		return err
	}

	// Assume L2 interface only
	result := &current.Result{
		CNIVersion: current.ImplementedSpecVersion,
		Interfaces: []*current.Interface{
			brInterface,
			hostInterface,
			containerInterface,
		},
	}
	file, err := os.OpenFile("/tmp/cni.log", os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0755)
	if err != nil {
		return err
	}
	defer file.Close()

	if n.MacSpoofChk {
		sc := link.NewSpoofChecker(hostInterface.Name, containerInterface.Mac, uniqueID(args.ContainerID, args.IfName))
		if err := sc.Setup(); err != nil {
			return err
		}
		defer func() {
			if !success {
				if err := sc.Teardown(); err != nil {
					fmt.Fprintf(os.Stderr, "%v", err)
				}
			}
		}()
	}

	ipt, err := iptables.NewWithProtocol(iptables.ProtocolIPv4)
	if err != nil {
		return fmt.Errorf("failed to open IPTables: %v", err)
	}

	fmt.Fprintf(file, "Is Layer3: %s\n", isLayer3)
	if isLayer3 {
		err = setupFirewallRules(ipt, hostInterface.Name)
		if err != nil {
			return fmt.Errorf("couldn't setup firewall rules: %v", err)
		}

		// run the IPAM plugin and get back the config to apply
		r, err := ipam.ExecAdd(n.IPAM.Type, args.StdinData)
		if err != nil {
			success = false
			return err
		}

		// release IP in case of failure
		defer func() {
			if !success {
				ipam.ExecDel(n.IPAM.Type, args.StdinData)
			}
		}()

		// Convert whatever the IPAM result was into the current Result type
		ipamResult, err := current.NewResultFromResult(r)
		if err != nil {
			return err
		}

		result.IPs = ipamResult.IPs
		result.Routes = ipamResult.Routes
		result.DNS = ipamResult.DNS

		if len(result.IPs) == 0 {
			return errors.New("IPAM plugin returned missing IP config")
		}

		// Gather gateway information for each IP family
		gwsV4, gwsV6, err := calcGateways(result, n)
		if err != nil {
			return err
		}

		// Configure the container hardware address and IP address(es)
		if err := netns.Do(func(_ ns.NetNS) error {
			if n.EnableDad {
				_, _ = sysctl.Sysctl(fmt.Sprintf("/net/ipv6/conf/%s/enhanced_dad", args.IfName), "1")
				_, _ = sysctl.Sysctl(fmt.Sprintf("net/ipv6/conf/%s/accept_dad", args.IfName), "1")
			} else {
				_, _ = sysctl.Sysctl(fmt.Sprintf("net/ipv6/conf/%s/accept_dad", args.IfName), "0")
			}
			_, _ = sysctl.Sysctl(fmt.Sprintf("net/ipv4/conf/%s/arp_notify", args.IfName), "1")

			// Add the IP to the interface
			if err := ipam.ConfigureIface(args.IfName, result); err != nil {
				return err
			}

			if n.EnableIPv6 {
				_, err = sysctl.Sysctl(fmt.Sprintf("net/ipv6/conf/%s/autoconf", args.IfName), "1")
				if err != nil {
					return fmt.Errorf("could not enable IPv6 autoconf on '%s': %v", args.IfName, err)
				}
				_, err = sysctl.Sysctl(fmt.Sprintf("net/ipv6/conf/%s/accept_ra", args.IfName), "1")
				if err != nil {
					return fmt.Errorf("could not enable IPv6 accept_ra on '%s': %v", args.IfName, err)
				}
				_, err = sysctl.Sysctl(fmt.Sprintf("net/ipv6/conf/%s/disable_ipv6", args.IfName), "0")
				if err != nil {
					return fmt.Errorf("could not enable IPv6 on '%s': %v", args.IfName, err)
				}
			}

			return nil
		}); err != nil {
			return err
		}

		// check bridge port state
		retries := []int{0, 50, 500, 1000, 1000}
		var hostVeth netlink.Link
		for idx, sleep := range retries {
			time.Sleep(time.Duration(sleep) * time.Millisecond)

			hostVeth, err = netlink.LinkByName(hostInterface.Name)
			if err != nil {
				return err
			}
			if hostVeth.Attrs().OperState == netlink.OperUp {
				break
			}

			if idx == len(retries)-1 {
				return fmt.Errorf("bridge port in error state: %s", hostVeth.Attrs().OperState)
			}
		}

		var contVeth *net.Interface
		if err := netns.Do(func(_ ns.NetNS) error {
			// Send a gratuitous arp
			contVeth, err = net.InterfaceByName(args.IfName)
			if err != nil {
				return err
			}
			return nil
		}); err != nil {
			return fmt.Errorf("failed to send gratuitous ARP: %v", err)
		}

		// Setup container routes
		uplinkAddrs, err := netlink.AddrList(br, netlink.FAMILY_V4)
		if err != nil {
			return fmt.Errorf("couldn't find IPv4 addresses for uplink interface: %v", err)
		}
		var gw6Ip net.IP
		if n.EnableIPv6 {
			uplink6Addrs, err := netlink.AddrList(br, netlink.FAMILY_V6)
			if err != nil {
				return fmt.Errorf("couldn't find IPv6 addresses for uplink interface: %v", err)
			}
			gw6Ip = uplink6Addrs[0].IP
		}

		gwIp := uplinkAddrs[0].IP
		err = netns.Do(func(_ ns.NetNS) error {
			containerLink, err := netlink.LinkByName(args.IfName)
			if err != nil {
				return fmt.Errorf("couldn't find interface '%s' even though we just created it: %v", args.IfName, err)
			}

			// Delete all routes. We're going to explicitly create our own routes the way we want
			routes, _ := netlink.RouteList(containerLink, netlink.FAMILY_ALL)
			for _, route := range routes {
				err = netlink.RouteDel(&route)
				if err != nil {
					return fmt.Errorf("couldn't delete all routes before setting up new routes: %v", err)
				}
			}

			// Add the local scope
			// This tells the container to forward everything to the host stack
			err = addRouteToHost(containerLink, gwIp, ipamResult.IPs[0].Address.IP)
			if err != nil {
				return fmt.Errorf("couldn't create ipv4 route in container to host: %v", err)
			}

			if n.EnableIPv6 {
				err = netlink.RouteAdd(&netlink.Route{
					LinkIndex: containerLink.Attrs().Index,
					Scope:     netlink.SCOPE_LINK,
					Dst:       netlink.NewIPNet(gw6Ip),
				})

				if err != nil {
					return fmt.Errorf("couldn't create ipv6 route in container to host for ip (%s): %v", gw6Ip, err)
				}

				for idx, sleep := range retries {
					containerIpv6, err := netlink.AddrList(containerLink, netlink.FAMILY_V6)
					if err != nil {
						return fmt.Errorf("couldn't get IPv6 addresses for container interface '%s': %v", args.IfName, err)
					}

					var foundAddr = false
					for _, addr := range containerIpv6 {
						if addr.Scope == int(netlink.SCOPE_UNIVERSE) {
							result.IPs = append(result.IPs, &current.IPConfig{
								Interface: &containerLink.Attrs().Index,
								Address:   *addr.IPNet,
							})
							foundAddr = true
							break
						}
					}
					if foundAddr {
						break
					}

					time.Sleep(time.Duration(sleep) * time.Millisecond)

					if idx == len(retries)-1 {
						return fmt.Errorf("timed out waiting for IPv6 autoconfig: %s", hostVeth.Attrs().OperState)
					}
				}
			}

			brMac, err := net.ParseMAC(brInterface.Mac)
			err = netlink.NeighSet(&netlink.Neigh{
				LinkIndex:    containerLink.Attrs().Index,
				Family:       netlink.FAMILY_V4,
				State:        netlink.NUD_PERMANENT,
				IP:           gwIp,
				HardwareAddr: brMac,
			})

			if err != nil {
				return fmt.Errorf("failed to add permanent neighbor of bridge to container interface: %v", err)
			}

			return nil
		})
		if err != nil {
			return fmt.Errorf("couldn't setup container routes: %v", err)
		}

		// Configure route from host to container
		for _, containerIp := range ipamResult.IPs {
			err = netlink.NeighSet(&netlink.Neigh{
				LinkIndex:    hostVeth.Attrs().Index,
				Family:       netlink.FAMILY_V4,
				State:        netlink.NUD_PERMANENT,
				IP:           containerIp.Address.IP,
				HardwareAddr: contVeth.HardwareAddr,
			})
			if err != nil {
				return fmt.Errorf("couldn't add ARP route from host to container: %v", err)
			}

			err = netlink.RouteAdd(&netlink.Route{
				LinkIndex: hostVeth.Attrs().Index,
				Dst:       netlink.NewIPNet(containerIp.Address.IP),
				Scope:     netlink.SCOPE_LINK,
			})

			if err != nil {
				return fmt.Errorf("couldn't route from host to container: %v", err)
			}
		}

		if n.IsGW {
			var firstV4Addr net.IP
			var vlanInterface *current.Interface
			// Set the IP address(es) on the bridge and enable forwarding
			for _, gws := range []*gwInfo{gwsV4, gwsV6} {
				for _, gw := range gws.gws {
					if gw.IP.To4() != nil && firstV4Addr == nil {
						firstV4Addr = gw.IP
					}
					if n.Vlan != 0 {
						vlanIface, err := ensureVlanInterface(br, n.Vlan)
						if err != nil {
							return fmt.Errorf("failed to create vlan interface: %v", err)
						}

						if vlanInterface == nil {
							vlanInterface = &current.Interface{Name: vlanIface.Attrs().Name,
								Mac: vlanIface.Attrs().HardwareAddr.String()}
							result.Interfaces = append(result.Interfaces, vlanInterface)
						}

						err = ensureAddr(vlanIface, gws.family, &gw, n.ForceAddress)
						if err != nil {
							return fmt.Errorf("failed to set vlan interface for bridge with addr: %v", err)
						}
					} else {
						err = ensureAddr(br, gws.family, &gw, n.ForceAddress)
						if err != nil {
							return fmt.Errorf("failed to set bridge addr: %v", err)
						}
					}
				}

				if gws.gws != nil {
					if err = enableIPForward(gws.family); err != nil {
						return fmt.Errorf("failed to enable forwarding: %v", err)
					}
				}
			}
		}

		if err = enableIPForward(netlink.FAMILY_V4); err != nil {
			return fmt.Errorf("failed to enable forwarding: %v", err)
		}
		if err = enableIPForward(netlink.FAMILY_V6); err != nil {
			return fmt.Errorf("failed to enable forwarding: %v", err)
		}

		if n.IPMasq {
			chain := utils.FormatChainName(n.Name, args.ContainerID)
			comment := utils.FormatComment(n.Name, args.ContainerID)
			for _, ipc := range result.IPs {
				if err = ip.SetupIPMasq(&ipc.Address, chain, comment); err != nil {
					return err
				}
			}
		}
	} else {
		if err := netns.Do(func(_ ns.NetNS) error {
			link, err := netlink.LinkByName(args.IfName)
			if err != nil {
				return fmt.Errorf("failed to retrieve link: %v", err)
			}
			// If layer 2 we still need to set the container veth to up
			if err = netlink.LinkSetUp(link); err != nil {
				return fmt.Errorf("failed to set %q up: %v", args.IfName, err)
			}
			return nil
		}); err != nil {
			return err
		}
	}

	// Refetch the bridge since its MAC address may change when the first
	// veth is added or after its IP address is set
	br, err = bridgeByName(n.BrName)
	if err != nil {
		return err
	}
	brInterface.Mac = br.Attrs().HardwareAddr.String()

	// Return an error requested by testcases, if any
	if debugPostIPAMError != nil {
		return debugPostIPAMError
	}

	// Use incoming DNS settings if provided, otherwise use the
	// settings that were already configued by the IPAM plugin
	if dnsConfSet(n.DNS) {
		result.DNS = n.DNS
	}

	success = true

	return types.PrintResult(result, cniVersion)
}

func addRouteToHost(containerLink netlink.Link, gwIp net.IP, srcAddress net.IP) error {
	err := netlink.RouteAdd(&netlink.Route{
		LinkIndex: containerLink.Attrs().Index,

		Scope: netlink.SCOPE_LINK,
		Dst:   netlink.NewIPNet(gwIp),
	})
	if err != nil {
		return fmt.Errorf("failed to add route: %s/32 scope link dev %s (container): %v", gwIp, containerLink.Attrs().Name, err)
	}
	err = netlink.RouteAdd(&netlink.Route{
		LinkIndex: containerLink.Attrs().Index,
		Gw:        gwIp,
		Dst: &net.IPNet{
			IP:   net.IPv4zero,
			Mask: net.CIDRMask(0, 0),
		},
		Src:      srcAddress,
		Priority: 1024,
	})

	// Temporarily ignore this. I think this breaks when running in a Multus environment because there's already another route
	/*if err != nil {
		return fmt.Errorf("failed to add route: next hop %s src %s dev %s (in container): %v", gwIp, srcAddress, containerLink.Attrs().Name, err)
	}*/

	return nil
}

func dnsConfSet(dnsConf types.DNS) bool {
	return dnsConf.Nameservers != nil ||
		dnsConf.Search != nil ||
		dnsConf.Options != nil ||
		dnsConf.Domain != ""
}

func cmdDel(args *skel.CmdArgs) error {
	n, _, err := loadNetConf(args.StdinData, args.Args)
	if err != nil {
		return err
	}

	isLayer3 := n.IPAM.Type != ""

	ipamDel := func() error {
		if isLayer3 {
			if err := ipam.ExecDel(n.IPAM.Type, args.StdinData); err != nil {
				return err
			}
		}
		return nil
	}

	if args.Netns == "" {
		return ipamDel()
	}

	// There is a netns so try to clean up. Delete can be called multiple times
	// so don't return an error if the device is already removed.
	// If the device isn't there then don't try to clean up IP masq either.
	var ipnets []*net.IPNet
	err = ns.WithNetNSPath(args.Netns, func(_ ns.NetNS) error {
		var err error
		ipnets, err = ip.DelLinkByNameAddr(args.IfName)
		if err != nil && err == ip.ErrLinkNotFound {
			return nil
		}
		return err
	})

	if err != nil {
		//  if NetNs is passed down by the Cloud Orchestration Engine, or if it called multiple times
		// so don't return an error if the device is already removed.
		// https://github.com/kubernetes/kubernetes/issues/43014#issuecomment-287164444
		_, ok := err.(ns.NSPathNotExistErr)
		if ok {
			return ipamDel()
		}
		return err
	}

	// call ipam.ExecDel after clean up device in netns
	if err := ipamDel(); err != nil {
		return err
	}

	if n.MacSpoofChk {
		sc := link.NewSpoofChecker("", "", uniqueID(args.ContainerID, args.IfName))
		if err := sc.Teardown(); err != nil {
			fmt.Fprintf(os.Stderr, "%v", err)
		}
	}

	if isLayer3 && n.IPMasq {
		chain := utils.FormatChainName(n.Name, args.ContainerID)
		comment := utils.FormatComment(n.Name, args.ContainerID)
		for _, ipn := range ipnets {
			if err := ip.TeardownIPMasq(ipn, chain, comment); err != nil {
				return err
			}
		}
	}

	return err
}

func main() {
	skel.PluginMain(cmdAdd, cmdCheck, cmdDel, version.All, bv.BuildString("bridge"))
}

type cniBridgeIf struct {
	Name        string
	ifIndex     int
	peerIndex   int
	masterIndex int
	found       bool
}

func validateInterface(intf current.Interface, expectInSb bool) (cniBridgeIf, netlink.Link, error) {

	ifFound := cniBridgeIf{found: false}
	if intf.Name == "" {
		return ifFound, nil, fmt.Errorf("Interface name missing ")
	}

	link, err := netlink.LinkByName(intf.Name)
	if err != nil {
		return ifFound, nil, fmt.Errorf("Interface name %s not found", intf.Name)
	}

	if expectInSb {
		if intf.Sandbox == "" {
			return ifFound, nil, fmt.Errorf("Interface %s is expected to be in a sandbox", intf.Name)
		}
	} else {
		if intf.Sandbox != "" {
			return ifFound, nil, fmt.Errorf("Interface %s should not be in sandbox", intf.Name)
		}
	}

	return ifFound, link, err
}

func validateCniBrInterface(intf current.Interface, n *NetConf) (cniBridgeIf, error) {

	brFound, link, err := validateInterface(intf, false)
	if err != nil {
		return brFound, err
	}

	_, isBridge := link.(*netlink.Bridge)
	if !isBridge {
		return brFound, fmt.Errorf("Interface %s does not have link type of bridge", intf.Name)
	}

	if intf.Mac != "" {
		if intf.Mac != link.Attrs().HardwareAddr.String() {
			return brFound, fmt.Errorf("Bridge interface %s Mac doesn't match: %s", intf.Name, intf.Mac)
		}
	}

	linkPromisc := link.Attrs().Promisc != 0
	if linkPromisc != n.PromiscMode {
		return brFound, fmt.Errorf("Bridge interface %s configured Promisc Mode %v doesn't match current state: %v ",
			intf.Name, n.PromiscMode, linkPromisc)
	}

	brFound.found = true
	brFound.Name = link.Attrs().Name
	brFound.ifIndex = link.Attrs().Index
	brFound.masterIndex = link.Attrs().MasterIndex

	return brFound, nil
}

func validateCniVethInterface(intf *current.Interface, brIf cniBridgeIf, contIf cniBridgeIf) (cniBridgeIf, error) {

	vethFound, link, err := validateInterface(*intf, false)
	if err != nil {
		return vethFound, err
	}

	_, isVeth := link.(*netlink.Veth)
	if !isVeth {
		// just skip it, it's not what CNI created
		return vethFound, nil
	}

	_, vethFound.peerIndex, err = ip.GetVethPeerIfindex(link.Attrs().Name)
	if err != nil {
		return vethFound, fmt.Errorf("Unable to obtain veth peer index for veth %s", link.Attrs().Name)
	}
	vethFound.ifIndex = link.Attrs().Index
	vethFound.masterIndex = link.Attrs().MasterIndex

	if vethFound.ifIndex != contIf.peerIndex {
		return vethFound, nil
	}

	if contIf.ifIndex != vethFound.peerIndex {
		return vethFound, nil
	}

	if vethFound.masterIndex != brIf.ifIndex {
		return vethFound, nil
	}

	if intf.Mac != "" {
		if intf.Mac != link.Attrs().HardwareAddr.String() {
			return vethFound, fmt.Errorf("Interface %s Mac doesn't match: %s not found", intf.Name, intf.Mac)
		}
	}

	vethFound.found = true
	vethFound.Name = link.Attrs().Name

	return vethFound, nil
}

func validateCniContainerInterface(intf current.Interface) (cniBridgeIf, error) {

	vethFound, link, err := validateInterface(intf, true)
	if err != nil {
		return vethFound, err
	}

	_, isVeth := link.(*netlink.Veth)
	if !isVeth {
		return vethFound, fmt.Errorf("Error: Container interface %s not of type veth", link.Attrs().Name)
	}
	_, vethFound.peerIndex, err = ip.GetVethPeerIfindex(link.Attrs().Name)
	if err != nil {
		return vethFound, fmt.Errorf("Unable to obtain veth peer index for veth %s", link.Attrs().Name)
	}
	vethFound.ifIndex = link.Attrs().Index

	if intf.Mac != "" {
		if intf.Mac != link.Attrs().HardwareAddr.String() {
			return vethFound, fmt.Errorf("Interface %s Mac %s doesn't match container Mac: %s", intf.Name, intf.Mac, link.Attrs().HardwareAddr)
		}
	}

	vethFound.found = true
	vethFound.Name = link.Attrs().Name

	return vethFound, nil
}

func cmdCheck(args *skel.CmdArgs) error {

	n, _, err := loadNetConf(args.StdinData, args.Args)
	if err != nil {
		return err
	}
	netns, err := ns.GetNS(args.Netns)
	if err != nil {
		return fmt.Errorf("failed to open netns %q: %v", args.Netns, err)
	}
	defer netns.Close()

	// run the IPAM plugin and get back the config to apply
	err = ipam.ExecCheck(n.IPAM.Type, args.StdinData)
	if err != nil {
		return err
	}

	// Parse previous result.
	if n.NetConf.RawPrevResult == nil {
		return fmt.Errorf("Required prevResult missing")
	}

	if err := version.ParsePrevResult(&n.NetConf); err != nil {
		return err
	}

	result, err := current.NewResultFromResult(n.PrevResult)
	if err != nil {
		return err
	}

	var errLink error
	var contCNI, vethCNI cniBridgeIf
	var brMap, contMap current.Interface

	// Find interfaces for names whe know, CNI Bridge and container
	for _, intf := range result.Interfaces {
		if n.BrName == intf.Name {
			brMap = *intf
			continue
		} else if args.IfName == intf.Name {
			if args.Netns == intf.Sandbox {
				contMap = *intf
				continue
			}
		}
	}

	brCNI, err := validateCniBrInterface(brMap, n)
	if err != nil {
		return err
	}

	// The namespace must be the same as what was configured
	if args.Netns != contMap.Sandbox {
		return fmt.Errorf("Sandbox in prevResult %s doesn't match configured netns: %s",
			contMap.Sandbox, args.Netns)
	}

	// Check interface against values found in the container
	if err := netns.Do(func(_ ns.NetNS) error {
		contCNI, errLink = validateCniContainerInterface(contMap)
		if errLink != nil {
			return errLink
		}
		return nil
	}); err != nil {
		return err
	}

	// Now look for veth that is peer with container interface.
	// Anything else wasn't created by CNI, skip it
	for _, intf := range result.Interfaces {
		// Skip this result if name is the same as cni bridge
		// It's either the cni bridge we dealt with above, or something with the
		// same name in a different namespace.  We just skip since it's not ours
		if brMap.Name == intf.Name {
			continue
		}

		// same here for container name
		if contMap.Name == intf.Name {
			continue
		}

		vethCNI, errLink = validateCniVethInterface(intf, brCNI, contCNI)
		if errLink != nil {
			return errLink
		}

		if vethCNI.found {
			// veth with container interface as peer and bridge as master found
			break
		}
	}

	if !brCNI.found {
		return fmt.Errorf("CNI created bridge %s in host namespace was not found", n.BrName)
	}
	if !contCNI.found {
		return fmt.Errorf("CNI created interface in container %s not found", args.IfName)
	}
	if !vethCNI.found {
		return fmt.Errorf("CNI veth created for bridge %s was not found", n.BrName)
	}

	// Check prevResults for ips, routes and dns against values found in the container
	if err := netns.Do(func(_ ns.NetNS) error {
		err = ip.ValidateExpectedInterfaceIPs(args.IfName, result.IPs)
		if err != nil {
			return err
		}

		err = ip.ValidateExpectedRoute(result.Routes)
		if err != nil {
			return err
		}
		return nil
	}); err != nil {
		return err
	}

	return nil
}

func uniqueID(containerID, cniIface string) string {
	return containerID + "-" + cniIface
}
