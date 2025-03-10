// Copyright 2021 CNI authors
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
	"fmt"

	"github.com/d2g/dhcp4"
	"github.com/d2g/dhcp4client"
)

const (
	MaxDHCPLen = 576
)

//Send the Discovery Packet to the Broadcast Channel
func DhcpSendDiscoverPacket(c *dhcp4client.Client, options dhcp4.Options) (dhcp4.Packet, error) {
	discoveryPacket := c.DiscoverPacket()

	for opt, data := range options {
		discoveryPacket.AddOption(opt, data)
	}

	discoveryPacket.PadToMinSize()
	return discoveryPacket, c.SendPacket(discoveryPacket)
}

//Send Request Based On the offer Received.
func DhcpSendRequest(c *dhcp4client.Client, options dhcp4.Options, offerPacket *dhcp4.Packet) (dhcp4.Packet, error) {
	requestPacket := c.RequestPacket(offerPacket)

	for opt, data := range options {
		requestPacket.AddOption(opt, data)
	}

	requestPacket.PadToMinSize()

	return requestPacket, c.SendPacket(requestPacket)
}

//Send Decline to the received acknowledgement.
func DhcpSendDecline(c *dhcp4client.Client, acknowledgementPacket *dhcp4.Packet, options dhcp4.Options) (dhcp4.Packet, error) {
	declinePacket := c.DeclinePacket(acknowledgementPacket)

	for opt, data := range options {
		declinePacket.AddOption(opt, data)
	}

	declinePacket.PadToMinSize()

	return declinePacket, c.SendPacket(declinePacket)
}

//Lets do a Full DHCP Request.
func DhcpRequest(c *dhcp4client.Client, options dhcp4.Options) (bool, dhcp4.Packet, error) {
	discoveryPacket, err := DhcpSendDiscoverPacket(c, options)
	if err != nil {
		return false, discoveryPacket, err
	}

	offerPacket, err := c.GetOffer(&discoveryPacket)
	if err != nil {
		return false, offerPacket, err
	}

	requestPacket, err := DhcpSendRequest(c, options, &offerPacket)
	if err != nil {
		return false, requestPacket, err
	}

	acknowledgement, err := c.GetAcknowledgement(&requestPacket)
	if err != nil {
		return false, acknowledgement, err
	}

	acknowledgementOptions := acknowledgement.ParseOptions()
	if dhcp4.MessageType(acknowledgementOptions[dhcp4.OptionDHCPMessageType][0]) != dhcp4.ACK {
		return false, acknowledgement, nil
	}

	return true, acknowledgement, nil
}

//Renew a lease backed on the Acknowledgement Packet.
//Returns Successful, The AcknowledgementPacket, Any Errors
func DhcpRenew(c *dhcp4client.Client, acknowledgement dhcp4.Packet, options dhcp4.Options) (bool, dhcp4.Packet, error) {
	renewRequest := c.RenewalRequestPacket(&acknowledgement)

	for opt, data := range options {
		renewRequest.AddOption(opt, data)
	}

	renewRequest.PadToMinSize()

	err := c.SendPacket(renewRequest)
	if err != nil {
		return false, renewRequest, err
	}

	newAcknowledgement, err := c.GetAcknowledgement(&renewRequest)
	if err != nil {
		return false, newAcknowledgement, err
	}

	newAcknowledgementOptions := newAcknowledgement.ParseOptions()
	if dhcp4.MessageType(newAcknowledgementOptions[dhcp4.OptionDHCPMessageType][0]) != dhcp4.ACK {
		msg := newAcknowledgementOptions[dhcp4.OptionMessage]
		return false, newAcknowledgement, fmt.Errorf("dhcp server responded: %s", msg)
	}

	return true, newAcknowledgement, nil
}

//Release a lease backed on the Acknowledgement Packet.
//Returns Any Errors
func DhcpRelease(c *dhcp4client.Client, acknowledgement dhcp4.Packet, options dhcp4.Options) error {
	release := c.ReleasePacket(&acknowledgement)

	for opt, data := range options {
		release.AddOption(opt, data)
	}

	release.PadToMinSize()

	return c.SendPacket(release)
}
