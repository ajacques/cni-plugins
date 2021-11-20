package main

import (
	"encoding/json"
	"fmt"
	"net"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/cni/pkg/version"
	"github.com/containernetworking/plugins/pkg/ns"
	bv "github.com/containernetworking/plugins/pkg/utils/buildversion"
	netlink "github.com/vishvananda/netlink"
)

type PluginConf struct {
	types.NetConf
	Master string `json:"master"`

	RuntimeConfig *struct {
		PodIp net.IP
	} `json:"runtimeConfig"`
}

// parseConfig parses the supplied configuration (and prevResult) from stdin.
func parseConfig(stdin []byte) (*PluginConf, error) {
	conf := PluginConf{}

	if err := json.Unmarshal(stdin, &conf); err != nil {
		return nil, fmt.Errorf("failed to parse network configuration: %v", err)
	}

	// Parse previous result. This will parse, validate, and place the
	// previous result object into conf.PrevResult. If you need to modify
	// or inspect the PrevResult you will need to convert it to a concrete
	// versioned Result struct.
	if err := version.ParsePrevResult(&conf.NetConf); err != nil {
		return nil, fmt.Errorf("could not parse prevResult: %v", err)
	}
	// End previous result parsing

	return &conf, nil
}

func getContainerInterface(interfaces []*current.Interface) (netlink.Link, error) {
	var err error
	var containerIFName string
	for _, netif := range interfaces {
		if netif.Sandbox != "" {
			containerIFName = netif.Name
			link, _ := netlink.LinkByName(netif.Name)
			routes, _ := netlink.RouteList(link, netlink.FAMILY_ALL)
			for _, route := range routes {
				err = netlink.RouteDel(&route)
				if err != nil {
					return nil, err
				}
			}
		}
	}

	return netlink.LinkByName(containerIFName)
}

// cmdAdd is called for ADD requests
func cmdAdd(args *skel.CmdArgs) error {
	conf, err := parseConfig(args.StdinData)
	if err != nil {
		return err
	}

	// A plugin can be either an "originating" plugin or a "chained" plugin.
	// Originating plugins perform initial sandbox setup and do not require
	// any result from a previous plugin in the chain. A chained plugin
	// modifies sandbox configuration that was previously set up by an
	// originating plugin and may optionally require a PrevResult from
	// earlier plugins in the chai
	// START chained plugin code
	if conf.PrevResult == nil {
		return fmt.Errorf("must be called as chained plugin")
	}

	// Convert the PrevResult to a concrete Result type that can be modified.
	prevResult, err := current.GetResult(conf.PrevResult)
	if err != nil {
		return fmt.Errorf("failed to convert prevResult: %v", err)
	}

	if len(prevResult.IPs) == 0 {
		return fmt.Errorf("got no container IPs")
	}

	// Pass the prevResult through this plugin to the next one
	result := prevResult

	// END chained plugin code

	// Implement your plugin here

	netns, _ := ns.GetNS(args.Netns)
	defer netns.Close()

	linkName := "mac0"

	macLink, err := netlink.LinkByName(linkName)
	if err != nil {
		return err
	}

	addrs, err := netlink.AddrList(macLink, netlink.FAMILY_V4)
	if err != nil {
		return fmt.Errorf("couldn't get addrs for interface '%s': %v", linkName, err)
	}
	gwIp := net.IPv4(169, 254, 1, 1)
	foundAddr := false
	for _, addr := range addrs {
		if addr.IP.Equal(gwIp) {
			foundAddr = true
			break
		}
	}
	if !foundAddr {
		err := netlink.AddrAdd(macLink, &netlink.Addr{
			IPNet: netlink.NewIPNet(gwIp),
		})
		if err != nil {
			return err
		}
	}

	containerIp := prevResult.IPs[0].Address.IP
	containerMac, err := net.ParseMAC(prevResult.Interfaces[0].Mac)
	if err != nil {
		return fmt.Errorf("couldn't parse MAC '%s': %v", prevResult.Interfaces[0].Mac, err)
	}

	err = netns.Do(func(_ ns.NetNS) error {
		dev, err := getContainerInterface(result.Interfaces)
		if err != nil {
			return err
		}

		// Add the local scope
		// This tells the container to forward everything to the host stack
		err = netlink.RouteAdd(&netlink.Route{
			LinkIndex: dev.Attrs().Index,
			Scope:     netlink.SCOPE_LINK,
			Dst:       netlink.NewIPNet(gwIp),
		})
		if err != nil {
			return err
		}

		err = netlink.RouteAdd(&netlink.Route{
			LinkIndex: dev.Attrs().Index,
			Gw:        gwIp,
			Src:       prevResult.IPs[0].Address.IP,
		})

		if err != nil {
			return err
		}

		err = netlink.NeighSet(&netlink.Neigh{
			LinkIndex: dev.Attrs().Index,
			Family: netlink.FAMILY_V4,
			State: netlink.NUD_PERMANENT,
			IP: gwIp,
			HardwareAddr: macLink.Attrs().HardwareAddr,
		})

		return err
	})
	if err != nil {
		return err
	}

	err = netlink.RouteAdd(&netlink.Route{
		LinkIndex: macLink.Attrs().Index,
		Dst: netlink.NewIPNet(containerIp),
		Scope: netlink.SCOPE_LINK,
	})

	if err != nil {
		return fmt.Errorf("couldn't route from host to container: %v", err)
	}

	err = netlink.NeighSet(&netlink.Neigh{
		LinkIndex: macLink.Attrs().Index,
		Family: netlink.FAMILY_V4,
		State: netlink.NUD_PERMANENT,
		IP: containerIp,
		HardwareAddr: containerMac,
	})
	if err != nil {
		return fmt.Errorf("couldn't add ARP route from host to container: %v", err)
	}

	// Pass through the result for the next plugin
	return types.PrintResult(result, conf.CNIVersion)
}

// cmdDel is called for DELETE requests
func cmdDel(args *skel.CmdArgs) error {
	_, err := parseConfig(args.StdinData)
	if err != nil {
		return err
	}

	netns, err := ns.GetNS(args.Netns)
	if err != nil {
		return err
	}
	defer netns.Close()

	var mainIp net.IP

	err = netns.Do(func(_ ns.NetNS) error {
		link, err := netlink.LinkByName(args.IfName)
		addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
		if err != nil {
			return err
		}

		if len(addrs) > 0 {
			mainIp = addrs[0].IP
		}
		return nil
	})
	if err != nil {
		return err
	}

	if mainIp != nil {
		macLink, err := netlink.LinkByName("mac0")
		err = netlink.RouteDel(&netlink.Route{
			LinkIndex: macLink.Attrs().Index,
			Dst:       netlink.NewIPNet(mainIp),
			Scope:     netlink.SCOPE_LINK,
		})
		if err != nil {
			return err
		}
	}

	return err
}

func main() {
	// replace TODO with your plugin name
	skel.PluginMain(cmdAdd, cmdCheck, cmdDel, version.All, bv.BuildString("route -fixer"))
}

func cmdCheck(args *skel.CmdArgs) error {
	// TODO: implement
	return fmt.Errorf("not implemented")
}
