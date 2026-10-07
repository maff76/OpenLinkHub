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

	server, err := zeroconf.Register(instance, serviceType, serviceDomain, cfg.ListenPort, txt, nil)
	if err != nil {
		logger.Log(logger.Fields{"error": err}).Warn("Unable to advertise OpenLinkHub via Zeroconf")
		return
	}
	mdnsServer = server
	logger.Log(logger.Fields{
		"instance": instance,
		"service":  serviceType + "." + serviceDomain,
		"port":     cfg.ListenPort,
	}).Info("OpenLinkHub Zeroconf discovery advertised")
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

func remotelyReachable(address string) bool {
	address = strings.TrimSpace(address)
	if address == "" || address == "0.0.0.0" || address == "::" || address == "[::]" {
		return true
	}
	ip := net.ParseIP(strings.Trim(address, "[]"))
	return ip == nil || !ip.IsLoopback()
}
