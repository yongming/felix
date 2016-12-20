// Copyright (c) 2016 Tigera, Inc. All rights reserved.
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

package ifacemonitor

import (
	log "github.com/Sirupsen/logrus"
	"github.com/projectcalico/felix/go/felix/set"
	"github.com/vishvananda/netlink"
	"syscall"
	"time"
)

type State string

const (
	StateUp   = "up"
	StateDown = "down"
)

type InterfaceStateCallback func(ifaceName string, ifaceState State)
type AddrStateCallback func(ifaceName string, addrs []string)

type InterfaceMonitor struct {
	upIfaces     set.Set
	Callback     InterfaceStateCallback
	AddrCallback AddrStateCallback
	ifaceName    map[int]string
	ifaceAddrs   map[int][]string
}

func New() *InterfaceMonitor {
	return &InterfaceMonitor{
		upIfaces:   set.New(),
		ifaceName:  make(map[int]string),
		ifaceAddrs: make(map[int][]string),
	}
}

func (m *InterfaceMonitor) MonitorInterfaces() {
	log.Info("Interface monitoring thread started.")
	updates := make(chan netlink.LinkUpdate)
	addr_updates := make(chan netlink.AddrUpdate)
	cancel := make(chan struct{})

	if err := netlink.LinkSubscribe(updates, cancel); err != nil {
		log.WithError(err).Fatal("Failed to subscribe to link updates")
	}
	if err := netlink.AddrSubscribe(addr_updates, cancel); err != nil {
		log.WithError(err).Fatal("Failed to subscribe to addr updates")
	}
	log.Info("Subscribed to netlink updates.")

	// Start of day, do a resync to notify all our existing interfaces.  We also do periodic
	// resyncs because it's not clear what the ordering guarantees are for our netlink
	// subscription vs a list operation as used by resync().
	err := m.resync()
	if err != nil {
		log.WithError(err).Fatal("Failed to read link states from netlink.")
	}

	// Schedule periodic resyncs after that.
	resyncTicker := time.NewTicker(10 * time.Second)
readLoop:
	for {
		select {
		case update, ok := <-updates:
			if !ok {
				break readLoop
			}
			m.handleNetlinkUpdate(update)
		case addr_update, ok := <-addr_updates:
			if !ok {
				break readLoop
			}
			m.handleNetlinkAddrUpdate(addr_update)
		case <-resyncTicker.C:
			err := m.resync()
			if err != nil {
				log.WithError(err).Fatal("Failed to read link states from netlink.")
			}
		}
	}
	log.Fatal("Failed to read events from Netlink.")
}

func (m *InterfaceMonitor) handleNetlinkUpdate(update netlink.LinkUpdate) {
	attrs := update.Attrs()
	if attrs == nil {
		// Defensive, some sort of interface that the netlink lib doesn't understand?
		log.WithField("update", update).Warn("Missing attributes on netlink update.")
		return
	}
	msgType := update.Header.Type
	ifaceExists := msgType == syscall.RTM_NEWLINK // Alternative is an RTM_DELLINK
	m.storeUpdateAndNotifyOnChange(ifaceExists, attrs)
}

func (m *InterfaceMonitor) handleNetlinkAddrUpdate(update netlink.AddrUpdate) {
	addr := update.LinkAddress.IP.String()
	ifIndex := update.LinkIndex
	exists := update.NewAddr
	log.WithFields(log.Fields{
		"addr":    addr,
		"ifIndex": ifIndex,
		"exists":  exists,
	}).Info("Netlink address update.")

	_, ifaceKnown := m.ifaceName[ifIndex]
	if !ifaceKnown {
		log.WithField("ifIndex", ifIndex).Warn("No known iface with this index.")
		return
	}

	if exists {
		if !m.addrKnownForIface(addr, ifIndex) {
			m.addAddrForIface(addr, ifIndex)
			m.notifyIfaceAddrs(ifIndex)
		}
	} else {
		if m.addrKnownForIface(addr, ifIndex) {
			m.delAddrForIface(addr, ifIndex)
			m.notifyIfaceAddrs(ifIndex)
		}
	}
}

func (m *InterfaceMonitor) addrKnownForIface(addr string, ifIndex int) bool {
	for _, known := range m.ifaceAddrs[ifIndex] {
		if addr == known {
			return true
		}
	}
	return false
}

func (m *InterfaceMonitor) addAddrForIface(addr string, ifIndex int) {
	m.ifaceAddrs[ifIndex] = append(m.ifaceAddrs[ifIndex], addr)
}

func (m *InterfaceMonitor) delAddrForIface(addr string, ifIndex int) {
	for i, known := range m.ifaceAddrs[ifIndex] {
		if addr == known {
			last := len(m.ifaceAddrs[ifIndex]) - 1
			m.ifaceAddrs[ifIndex][i] = m.ifaceAddrs[ifIndex][last]
			m.ifaceAddrs[ifIndex] = m.ifaceAddrs[ifIndex][:last]
			break
		}
	}
}

func (m *InterfaceMonitor) notifyIfaceAddrs(ifIndex int) {
	m.AddrCallback(m.ifaceName[ifIndex], m.ifaceAddrs[ifIndex])
}

func (m *InterfaceMonitor) storeUpdateAndNotifyOnChange(ifaceExists bool, attrs *netlink.LinkAttrs) {
	// Store or remove mapping between this interface's index and name.
	if ifaceExists {
		m.ifaceName[attrs.Index] = attrs.Name
	} else {
		delete(m.ifaceName, attrs.Index)
	}
	// We need the operstate of the interface; this is carried in the IFF_RUNNING flag.
	// The IFF_UP flag contains the admin state, which doesn't tell us whether we can
	// program routes etc.
	rawFlags := attrs.RawFlags
	ifaceIsUp := ifaceExists && rawFlags&syscall.IFF_RUNNING != 0
	ifaceName := attrs.Name
	ifaceWasUp := m.upIfaces.Contains(ifaceName)
	logCxt := log.WithField("ifaceName", ifaceName)
	if ifaceIsUp && !ifaceWasUp {
		logCxt.Debug("Interface now up")
		m.upIfaces.Add(ifaceName)
		m.Callback(ifaceName, StateUp)
	} else if ifaceWasUp && !ifaceIsUp {
		logCxt.Debug("Interface now down")
		m.upIfaces.Discard(ifaceName)
		m.Callback(ifaceName, StateDown)
	}
}

func (m *InterfaceMonitor) resync() error {
	log.Debug("Resyncing interface state.")
	links, err := netlink.LinkList()
	if err != nil {
		log.WithError(err).Warn("Netlink list operation failed.")
		return err
	}
	currentIfaces := set.New()
	for _, link := range links {
		attrs := link.Attrs()
		if attrs == nil {
			// Defensive, some sort of interface that the netlink lib doesn't
			// understand?
			log.WithField("link", link).Warn("Missing attributes on netlink update.")
			continue
		}
		currentIfaces.Add(attrs.Name)
		m.storeUpdateAndNotifyOnChange(true, attrs)

		ifIndex := attrs.Index
		new_addrs := []string{}
		for _, family := range [2]int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
			addrs, err := netlink.AddrList(link, family)
			if err != nil {
				log.WithError(err).Warn("Netlink addr list operation failed.")
				return err
			}
			for _, addr := range addrs {
				new_addrs = append(new_addrs, addr.IPNet.IP.String())
			}
		}
		m.ifaceAddrs[ifIndex] = new_addrs
		m.notifyIfaceAddrs(ifIndex)
	}
	m.upIfaces.Iter(func(name interface{}) error {
		if currentIfaces.Contains(name) {
			return nil
		}
		log.WithField("ifaceName", name).Info("Spotted interface removal on resync.")
		return set.RemoveItem
	})
	return nil
}
