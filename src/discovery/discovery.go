package discovery

// Package: discovery
// License: GPL-3.0 or later

import (
	"OpenLinkHub/src/config"
	"OpenLinkHub/src/logger"
	"OpenLinkHub/src/version"
	"fmt"
	"net"
	"strings"
	"sync"

	"github.com/grandcat/zeroconf"
)

const (
	serviceType   = "_openlinkhub._tcp"
	serviceDomain = "local."
)

var (
	mu         sync.Mutex
	mdnsServer *zeroconf.Server
)

// Start advertises the running OpenLinkHub HTTP API using DNS-SD/mDNS.
// Discovery is optional: failure to advertise must never prevent device control.
func Start() {
	mu.Lock()
	defer mu.Unlock()

	if mdnsServer != nil {
		return
	}

	cfg := config.GetConfig()
	if cfg.ListenPort <= 0 || cfg.InstanceID == "" {
		return
	}
	if !remotelyReachable(cfg.ListenAddress) {
		logger.Log(logger.Fields{"listenAddress": cfg.ListenAddress}).Debug("Zeroconf discovery disabled for loopback-only REST listener")
		return
	}

	buildVersion := "0.0.0"
	if build := version.GetBuildInfo(); build != nil && build.BuildVersion != "" {
		buildVersion = build.BuildVersion
	}

	shortID := cfg.InstanceID
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}
	instance := fmt.Sprintf("OpenLinkHub-%s", shortID)
	txt := []string{
		"id=" + cfg.InstanceID,
		"version=" + buildVersion,
		"api=1",
	}

	server, advertisedIP, advertisedInterface, err := register(instance, cfg.ListenAddress, cfg.ListenPort, txt)
	if err != nil {
		logger.Log(logger.Fields{"error": err}).Warn("Unable to advertise OpenLinkHub via Zeroconf")
		return
	}
	mdnsServer = server

	fields := logger.Fields{
		"instance": instance,
		"service":  serviceType + "." + serviceDomain,
		"port":     cfg.ListenPort,
	}
	if advertisedIP != "" {
		fields["address"] = advertisedIP
	}
	if advertisedInterface != "" {
		fields["interface"] = advertisedInterface
	}
	logger.Log(fields).Info("OpenLinkHub Zeroconf discovery advertised")
}

// Stop withdraws the mDNS service advertisement.
func Stop() {
	mu.Lock()
	defer mu.Unlock()
	if mdnsServer == nil {
		return
	}
	mdnsServer.Shutdown()
	mdnsServer = nil
}

// register prefers a single routable IPv4 address and its owning interface.
// This avoids publishing Docker, libvirt and unrelated link-local addresses on
// multi-homed hosts. If no suitable address can be selected, registration
// falls back to zeroconf's normal host-wide discovery so mDNS remains optional
// rather than becoming a startup dependency.
func register(instance, listenAddress string, port int, txt []string) (*zeroconf.Server, string, string, error) {
	ip, iface, err := advertisementTarget(listenAddress)
	if err == nil {
		host := fmt.Sprintf("openlinkhub-%s.local.", strings.ToLower(strings.TrimPrefix(instance, "OpenLinkHub-")))
		server, registerErr := zeroconf.RegisterProxy(
			instance,
			serviceType,
			serviceDomain,
			port,
			host,
			[]string{ip.String()},
			txt,
			[]net.Interface{*iface},
		)
		if registerErr == nil {
			return server, ip.String(), iface.Name, nil
		}
		logger.Log(logger.Fields{
			"address":   ip.String(),
			"interface": iface.Name,
			"error":     registerErr,
		}).Warn("Unable to advertise OpenLinkHub on selected Zeroconf interface; falling back to host-wide advertisement")
	} else {
		logger.Log(logger.Fields{"error": err}).Warn("Unable to select a Zeroconf LAN interface; falling back to host-wide advertisement")
	}

	server, registerErr := zeroconf.Register(instance, serviceType, serviceDomain, port, txt, nil)
	return server, "", "", registerErr
}

func advertisementTarget(listenAddress string) (net.IP, *net.Interface, error) {
	address := strings.TrimSpace(strings.Trim(listenAddress, "[]"))
	if address != "" && address != "0.0.0.0" && address != "::" {
		if ip := net.ParseIP(address); ip != nil {
			if ip4 := ip.To4(); ip4 != nil && !ip4.IsLoopback() {
				iface, err := interfaceForIP(ip4)
				if err != nil {
					return nil, nil, err
				}
				return ip4, iface, nil
			}
		}
	}

	// A UDP dial does not send application data; it asks the kernel which local
	// address it would use for the normal IPv4 default route. That reliably
	// avoids Docker/libvirt bridge addresses on typical multi-interface hosts.
	conn, err := net.Dial("udp4", "1.1.1.1:53")
	if err == nil {
		localIP := conn.LocalAddr().(*net.UDPAddr).IP.To4()
		_ = conn.Close()
		if localIP != nil && !localIP.IsLoopback() {
			iface, ifaceErr := interfaceForIP(localIP)
			if ifaceErr == nil {
				return localIP, iface, nil
			}
		}
	}

	return nil, nil, fmt.Errorf("unable to determine the IPv4 address used by the default route")
}

func interfaceForIP(target net.IP) (*net.Interface, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	for i := range interfaces {
		iface := &interfaces[i]
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagMulticast == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addresses, addrErr := iface.Addrs()
		if addrErr != nil {
			continue
		}
		for _, address := range addresses {
			var ip net.IP
			switch value := address.(type) {
			case *net.IPNet:
				ip = value.IP
			case *net.IPAddr:
				ip = value.IP
			}
			if ip != nil && ip.Equal(target) {
				return iface, nil
			}
		}
	}
	return nil, fmt.Errorf("no active multicast interface owns address %s", target.String())
}

func remotelyReachable(address string) bool {
	address = strings.TrimSpace(address)
	if address == "" || address == "0.0.0.0" || address == "::" || address == "[::]" {
		return true
	}
	ip := net.ParseIP(strings.Trim(address, "[]"))
	return ip == nil || !ip.IsLoopback()
}
