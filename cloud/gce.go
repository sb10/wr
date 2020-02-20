// Copyright © 2019 Genome Research Limited
// Author: Sendu Bala <sb10@sanger.ac.uk>.
//
//  This file is part of wr.
//
//  wr is free software: you can redistribute it and/or modify
//  it under the terms of the GNU Lesser General Public License as published by
//  the Free Software Foundation, either version 3 of the License, or
//  (at your option) any later version.
//
//  wr is distributed in the hope that it will be useful,
//  but WITHOUT ANY WARRANTY; without even the implied warranty of
//  MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
//  GNU Lesser General Public License for more details.
//
//  You should have received a copy of the GNU Lesser General Public License
//  along with wr. If not, see <http://www.gnu.org/licenses/>.

package cloud

// This file contains a provideri implementation for Google Compute Engine

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/VertebrateResequencing/wr/internal"
	"github.com/VividCortex/ewma"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/servers"
	"github.com/hashicorp/go-multierror"
	"github.com/inconshreveable/log15"
	"github.com/jpillora/backoff"
	"google.golang.org/api/compute/v1"
)

// gceEnvs contains the environment variable name we need to connect to GCE. No
// others are needed.
var gceEnvs = [...]string{"GOOGLE_APPLICATION_CREDENTIALS"}

// publicImageProjects are all the linux public image projects as of 2019.
// *** is there a way to query these instead?
var publicImageProjects = [...]string{"gce-uefi-images", "centos-cloud", "cos-cloud", "coreos-cloud", "debian-cloud", "rhel-cloud", "rhel-sap-cloud", "suse-cloud", "suse-sap-cloud", "ubuntu-os-cloud"}

// gcep is our implementer of provideri
type gcep struct {
	lastFlavorCache   time.Time
	externalNetworkID string
	networkName       string
	networkUUID       string
	ownName           string
	poolName          string
	securityGroup     string
	spawnTimes        ewma.MovingAverage
	spawnTimesVolume  ewma.MovingAverage
	tenantID          string
	log15.Logger
	computeClient   *compute.Service
	errorBackoff    *backoff.Backoff
	fmap            map[string]*Flavor
	imap            map[string]*compute.Image
	ipNet           *net.IPNet
	ownServer       *servers.Server
	fmapMutex       sync.RWMutex
	imapMutex       sync.RWMutex
	createdKeyPair  bool
	useConfigDrive  bool
	hasDefaultGroup bool
	spawnFailed     bool
}

// requiredEnv returns envs that are definitely required.
func (p *gcep) requiredEnv() []string {
	return gceEnvs[:]
}

// maybeEnv returns envs that might be required; in gce's case, this is nothing.
func (p *gcep) maybeEnv() []string {
	return nil
}

// initialize uses our required environment variable to authenticate with
// GCE and create a client we will use in the other methods.
func (p *gcep) initialize(logger log15.Logger) error {
	p.Logger = logger.New("cloud", "gce")

	// make a compute client
	var err error
	// var scopes = []string{"https://www.googleapis.com/auth/compute", "https://www.googleapis.com/auth/devstorage.full_control"}
	p.computeClient, err = compute.NewService(context.Background())
	if err != nil {
		return err
	}

	// flavors and images are retrieved on-demand via caching methods that store
	// in these maps
	p.fmap = make(map[string]*Flavor)
	p.imap = make(map[string]*compute.Image)

	// to get a reasonable new server timeout we'll keep track of how long it
	// takes to spawn them using an exponentially weighted moving average. We
	// keep track of servers spawned with and without volumes separately, since
	// volume creation takes much longer.
	p.spawnTimes = ewma.NewMovingAverage()
	p.spawnTimesVolume = ewma.NewMovingAverage()

	// spawn() backs off on new requests if the previous one failed, tracked
	// with a Backoff
	p.errorBackoff = &backoff.Backoff{
		Min:    1 * time.Second,
		Max:    initialServerSpawnTimeout,
		Factor: 3,
		Jitter: true,
	}

	return err
}

// cacheFlavors retrieves the current list of flavors from GCE and caches them
// in p. Old no-longer existent flavors are kept forever, so we can still see
// what resources old instances are using.
func (p *gcep) cacheFlavors() error {
	p.fmapMutex.Lock()
	defer func() {
		p.lastFlavorCache = time.Now()
		p.fmapMutex.Unlock()
	}()

	// pager := flavors.ListDetail(p.computeClient, flavors.ListOpts{})
	// return pager.EachPage(func(page pagination.Page) (bool, error) {
	// 	flavorList, err := flavors.ExtractFlavors(page)
	// 	if err != nil {
	// 		return false, err
	// 	}

	// 	for _, f := range flavorList {
	// 		p.fmap[f.ID] = &Flavor{
	// 			ID:    f.ID,
	// 			Name:  f.Name,
	// 			Cores: f.VCPUs,
	// 			RAM:   f.RAM,
	// 			Disk:  f.Disk,
	// 		}
	// 	}
	// 	return true, nil
	// })
	return nil
}

// getFlavor retrieves the desired flavor by id from the cache. If it's not in
// the cache, will call cacheFlavors() to get any newly added flavors. If still
// not in the cache, returns nil and an error.
func (p *gcep) getFlavor(flavorID string) (*Flavor, error) {
	p.fmapMutex.RLock()
	flavor, found := p.fmap[flavorID]
	p.fmapMutex.RUnlock()
	if !found {
		err := p.cacheFlavors()
		if err != nil {
			return nil, err
		}

		p.fmapMutex.RLock()
		flavor, found = p.fmap[flavorID]
		p.fmapMutex.RUnlock()
		if !found {
			return nil, errors.New(invalidFlavorIDMsg + ": " + flavorID)
		}
	}
	return flavor, nil
}

// cacheImages retrieves the current list of images from GCE and caches them in
// p. Old no-longer existent images are kept forever, so we can still see what
// images old instances are using.
func (p *gcep) cacheImages() error {
	p.imapMutex.Lock()
	defer p.imapMutex.Unlock()
	// *** need to know user's own project name to also list images in that
	for _, project := range publicImageProjects {
		imageList, err := p.computeClient.Images.List(project).Do()
		if err != nil {
			return err
		}
		if imageList != nil {
			for _, image := range imageList.Items {
				if image.Status != "READY" || (image.Deprecated != nil && image.Deprecated.State != "ACTIVE") {
					continue
				}
				p.imap[image.Family] = image
				p.imap[image.Name] = image
			}
		}
	}
	return nil
}

// getImage retrieves the desired image by name prefix from the cache. If it's
// not in the cache, will call cacheImages() to get any newly added images. If
// still not in the cache, returns nil and an error.
func (p *gcep) getImage(prefix string) (*compute.Image, error) {
	image := p.getImageFromCache(prefix)
	if image != nil {
		return image, nil
	}

	err := p.cacheImages()
	if err != nil {
		return nil, err
	}

	image = p.getImageFromCache(prefix)
	if image != nil {
		return image, nil
	}

	return nil, errors.New("no image with prefix [" + prefix + "] was found")
}

// getImageFromCache is used by getImage(); don't call this directly.
func (p *gcep) getImageFromCache(prefix string) *compute.Image {
	p.imapMutex.RLock()
	defer p.imapMutex.RUnlock()

	// find an exact match
	if image, found := p.imap[prefix]; found {
		return image
	}

	// failing that, find a random prefix match
	for _, image := range p.imap {
		if strings.HasPrefix(image.Name, prefix) {
			return image
		}
	}
	return nil
}

// deploy achieves the aims of Deploy().
func (p *gcep) deploy(resources *Resources, requiredPorts []int, useConfigDrive bool, gatewayIP, cidr string, dnsNameServers []string) error {
	// the resource name can only contain letters, numbers, underscores,
	// spaces and hyphens
	if !openstackValidResourceNameRegexp.MatchString(resources.ResourceName) {
		return Error{"openstack", "deploy", ErrBadResourceName}
	}

	// spawn() needs to figure out which of a server's ips are local, so we
	// parse and store the CIDR
	var err error
	_, p.ipNet, err = net.ParseCIDR(cidr)
	if err != nil {
		return err
	}

	// get/create key pair

	//resources.Details["keypair"] = kp.Name

	if len(requiredPorts) > 0 {
		// get/create security group, and see if there's a default group

		// resources.Details["secgroup"] = group.ID
		// p.securityGroup = resources.ResourceName
		// p.hasDefaultGroup = defaultGroupExists
	}

	// don't create any more resources if we're already running in GCE
	if p.inCloud() {
		return nil
	}

	// get/create network

	// resources.Details["network"] = networkID
	// p.networkName = resources.ResourceName
	// p.networkUUID = networkID

	// get/create subnet

	//resources.Details["subnet"] = subnetID

	// get/create router

	//resources.Details["router"] = routerID

	return err
}

// getCurrentServers returns details of other servers with the given resource
// name prefix.
func (p *gcep) getCurrentServers(resources *Resources) ([][]string, error) {
	var sdetails [][]string
	// pager := servers.List(p.computeClient, servers.ListOpts{})
	// err := pager.EachPage(func(page pagination.Page) (bool, error) {
	// 	serverList, err := servers.ExtractServers(page)
	// 	if err != nil {
	// 		return false, err
	// 	}

	// 	for _, server := range serverList {
	// 		if p.ownName != server.Name && strings.HasPrefix(server.Name, resources.ResourceName) {
	// 			serverIP, errg := p.getServerIP(server.ID)
	// 			if errg != nil {
	// 				continue
	// 			}

	// 			details := []string{server.ID, serverIP, server.Name, server.AdminPass}
	// 			sdetails = append(sdetails, details)
	// 		}
	// 	}

	// 	return true, nil
	// })
	// return sdetails, err
	return sdetails, nil
}

// inCloud checks if we're currently running on an GCE server based on our
// hostname matching a host in GCE.
func (p *gcep) inCloud() bool {
	// hostname, err := os.Hostname()
	inCloud := false
	// if err == nil {
	// 	pager := servers.List(p.computeClient, servers.ListOpts{})
	// 	err = pager.EachPage(func(page pagination.Page) (bool, error) {
	// 		serverList, errf := servers.ExtractServers(page)
	// 		if errf != nil {
	// 			return false, errf
	// 		}

	// 		for _, server := range serverList {
	// 			if nameToHostName(server.Name) == hostname {
	// 				p.ownName = hostname
	// 				server := server // pin (not needed since we return, but just to be careful)
	// 				p.ownServer = &server
	// 				inCloud = true
	// 				return false, nil
	// 			}
	// 		}

	// 		return true, nil
	// 	})

	// 	if err != nil {
	// 		p.Warn("paging through servers failed", "err", err)
	// 	}
	// }

	return inCloud
}

// flavors returns all our flavors.
func (p *gcep) flavors() map[string]*Flavor {
	// update the cached flavors at most once every half hour
	p.fmapMutex.RLock()
	if time.Since(p.lastFlavorCache) > 30*time.Minute {
		p.fmapMutex.RUnlock()
		err := p.cacheFlavors()
		if err != nil {
			p.Warn("failed to cache available flavors", "err", err)
		}
		p.fmapMutex.RLock()
	}
	fmap := make(map[string]*Flavor)
	for key, val := range p.fmap {
		fmap[key] = val
	}
	p.fmapMutex.RUnlock()
	return fmap
}

// getQuota achieves the aims of GetQuota().
func (p *gcep) getQuota() (*Quota, error) {
	// query our quota

	return nil, nil
}

// spawn achieves the aims of Spawn()
func (p *gcep) spawn(resources *Resources, osPrefix string, flavorID string, diskGB int, externalIP bool, usingQuotaCh chan bool) (serverID, serverIP, serverName, adminPass string, err error) {
	// get the image that matches desired OS
	image, err := p.getImage(osPrefix)
	if err != nil {
		return serverID, serverIP, serverName, adminPass, err
	}

	// flavor, err := p.getFlavor(flavorID)
	// if err != nil {
	// 	return serverID, serverIP, serverName, adminPass, err
	// }

	// if the OS image itself specifies a minimum disk size and it's higher than
	// requested disk, increase our requested disk
	if int(image.DiskSizeGb) > diskGB {
		diskGB = int(image.DiskSizeGb)
	}

	// if we previously had a problem spawning a server, wait before attempting
	// again
	if p.spawnFailed {
		time.Sleep(p.errorBackoff.Duration())
	}

	// we'll use the security group we created, and the "default" one if it
	// exists
	var secGroups []string
	if p.securityGroup != "" {
		secGroups = append(secGroups, p.securityGroup)
		if p.hasDefaultGroup {
			secGroups = append(secGroups, "default")
		}
	}

	// create the server with a unique name
	// var server *servers.Server
	// serverName = uniqueResourceName(resources.ResourceName)
	// createOpts := servers.CreateOpts{
	// 	Name:           serverName,
	// 	FlavorRef:      flavorID,
	// 	ImageRef:       image.ID,
	// 	SecurityGroups: secGroups,
	// 	Networks:       []servers.Network{{UUID: p.networkUUID}},
	// 	ConfigDrive:    &p.useConfigDrive,
	// 	UserData:       sentinelInitScript,
	// }
	var createdVolume bool
	// if diskGB > flavor.Disk {
	// 	server, err = bootfromvolume.Create(p.computeClient, keypairs.CreateOptsExt{
	// 		CreateOptsBuilder: bootfromvolume.CreateOptsExt{
	// 			CreateOptsBuilder: createOpts,
	// 			BlockDevice: []bootfromvolume.BlockDevice{
	// 				{
	// 					UUID:                image.ID,
	// 					SourceType:          bootfromvolume.SourceImage,
	// 					DeleteOnTermination: true,
	// 					DestinationType:     bootfromvolume.DestinationVolume,
	// 					VolumeSize:          diskGB,
	// 				},
	// 			},
	// 		},
	// 		KeyName: resources.ResourceName,
	// 	}).Extract()
	// 	createdVolume = true
	// } else {
	// 	server, err = servers.Create(p.computeClient, keypairs.CreateOptsExt{
	// 		CreateOptsBuilder: createOpts,
	// 		KeyName:           resources.ResourceName,
	// 	}).Extract()
	// }

	usingQuotaCh <- true

	if err != nil {
		p.spawnFailed = true
		return serverID, serverIP, serverName, adminPass, err
	}

	// wait for it to come up; servers.WaitForStatus has a timeout, but it
	// doesn't always work, so we roll our own
	waitForActive := make(chan error)
	go func() {
		defer internal.LogPanic(p.Logger, "spawn", false)

		var timeoutS float64
		if createdVolume {
			timeoutS = p.spawnTimesVolume.Value() * 4
		} else {
			timeoutS = p.spawnTimes.Value() * 4
		}
		if timeoutS <= 0 {
			timeoutS = initialServerSpawnTimeout.Seconds()
		}
		if timeoutS < 90 {
			timeoutS = 90
		}
		timeout := time.After(time.Duration(timeoutS) * time.Second)
		ticker := time.NewTicker(1 * time.Second)
		// start := time.Now()
		for {
			select {
			case <-ticker.C:
				// check current status of server

				// if current.Status == "ACTIVE" {
				// 	ticker.Stop()
				// 	spawnSecs := time.Since(start).Seconds()
				// 	if createdVolume {
				// 		p.spawnTimesVolume.Add(spawnSecs)
				// 	} else {
				// 		p.spawnTimes.Add(spawnSecs)
				// 	}
				// 	waitForActive <- nil
				// 	return
				// }
				// if current.Status == "ERROR" {
				// 	ticker.Stop()
				// 	msg := current.Fault.Message
				// 	if msg == "" {
				// 		msg = "the server is in ERROR state following an unknown problem"
				// 	}
				// 	waitForActive <- errors.New(msg)
				// 	return
				// }
				continue
			case <-timeout:
				ticker.Stop()
				waitForActive <- errors.New("timed out waiting for server to become ACTIVE")
				return
			}
		}
	}()
	err = <-waitForActive
	if err != nil {
		// since we're going to return an error that we failed to spawn, try and
		// delete the bad server in case it is still there
		p.spawnFailed = true
		// delerr := servers.Delete(p.computeClient, server.ID).ExtractErr()
		// if delerr != nil {
		// 	err = fmt.Errorf("%s\nadditionally, there was an error deleting the bad server: %s", err, delerr)
		// }
		return serverID, serverIP, serverName, adminPass, err
	}
	if p.spawnFailed {
		p.errorBackoff.Reset()
	}
	p.spawnFailed = false

	// *** NB. it can still take some number of seconds before I can ssh to it

	// serverID = server.ID
	// adminPass = server.AdminPass

	// get the servers IP; if we error for any reason we'll delete the server
	// first, because without an IP it's useless
	if externalIP {
		// give it a floating ip

		//serverIP = floatingIP
	} else {
		var errg error
		serverIP, errg = p.getServerIP(serverID)
		if errg != nil {
			errd := p.destroyServer(serverID)
			if errd != nil {
				p.Warn("server destruction after not finding ip", "server", serverID, "err", errd)
			}
			return serverID, serverIP, serverName, adminPass, errg
		}
	}

	return serverID, serverIP, serverName, adminPass, err
}

// errIsNoHardware returns true if error contains "There are not enough hosts
// available".
func (p *gcep) errIsNoHardware(err error) bool {
	return strings.Contains(err.Error(), "There are not enough hosts available")
}

// getServerIP tries to find the auto-assigned internal ip address of the server
// with the given ID.
func (p *gcep) getServerIP(serverID string) (string, error) {

	return "", nil
}

// checkServer achieves the aims of CheckServer()
func (p *gcep) checkServer(serverID string) (bool, error) {

	return false, nil //server.Status == "ACTIVE", nil
}

// destroyServer achieves the aims of DestroyServer()
func (p *gcep) destroyServer(serverID string) error {

	return nil
}

// tearDown achieves the aims of TearDown()
func (p *gcep) tearDown(resources *Resources) error {
	// throughout we'll ignore errors because we want to try and delete
	// as much as possible; we'll end up returning a concatenation of all of
	// them though
	var merr *multierror.Error

	// delete servers, except for ourselves

	if p.ownName == "" {
		// delete router

		// delete network (and its subnet)

		// delete secgroup

	}

	// delete keypair, unless we're running in OpenStack and securityGroup and
	// keypair have the same resourcename, indicating our current server needs
	// the same keypair we used to spawn our servers. Bypass the exception if
	// we definitely created the key pair this session

	return merr.ErrorOrNil()
}
