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

	linkName := prevResult.Interfaces[2].Name
	containerNet := prevResult.IPs[0].Address

	err = netns.Do(func(_ ns.NetNS) error {
		containerLink, err := netlink.LinkByName(linkName)
		if err != nil {
			return fmt.Errorf("couldn't find link (%s) in container netns: %v", linkName, err)
		}

		route := &netlink.Route{
			LinkIndex: containerLink.Attrs().Index,
			Scope:     netlink.SCOPE_LINK,
			Src:       containerNet.IP,
			Dst: &net.IPNet{
				IP:   containerNet.IP.Mask(containerNet.Mask),
				Mask: containerNet.Mask,
			},
		}

		err = netlink.RouteAdd(route)
		if err != nil {
			return fmt.Errorf("couldn't create route (%s) in container: %v", route, err)
		}

		_, i, err := net.ParseCIDR("224.0.0.0/4")
		if err != nil {
			return err
		}

		mcastroute := &netlink.Route{
			LinkIndex: containerLink.Attrs().Index,
			Scope:     netlink.SCOPE_LINK,
			Src:       containerNet.IP,
			Dst:       i,
		}

		err = netlink.RouteAdd(mcastroute)
		if err != nil {
			return fmt.Errorf("couldn't create route (%s) in container: %v", mcastroute, err)
		}

		return nil
	})
	if err != nil {
		return err
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

	return nil
}

func main() {
	// replace TODO with your plugin name
	skel.PluginMain(cmdAdd, cmdCheck, cmdDel, version.All, bv.BuildString("route -fixer"))
}

func cmdCheck(args *skel.CmdArgs) error {
	// TODO: implement
	return fmt.Errorf("not implemented")
}
