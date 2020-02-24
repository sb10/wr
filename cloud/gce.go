// Copyright © 2019-2020 Genome Research Limited
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
	ipNet           *net.IPNet
	ownServer       *servers.Server
	createdKeyPair  bool
	useConfigDrive  bool
	hasDefaultGroup bool
	spawnFailed     bool
	flavorCache     *Cache
	imageCache      *Cache
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

	// flavors and images are retrieved on-demand via cache
	p.flavorCache = NewCache(p.getFlavors, CacheDefaultRefresh)
	p.imageCache = NewCache(p.getImages, CacheDefaultRefresh)
	p.imageCache.SetPrefixCallback(p.prefixMatchImage)

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

// getFlavors is a CurrentResourceCallback for getting the current list of
// flavors from GCE.
func (p *gcep) getFlavors(resourceMap map[string]interface{}) error {
	// *** not yet implemented
	return nil
}

// getImages is a CurrentResourceCallback for getting the current list of
// images from GCE.
func (p *gcep) getImages(resourceMap map[string]interface{}) error {
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
				resourceMap[image.Family] = image
				resourceMap[image.Name] = image
			}
		}
	}
	return nil
}

// getImage retrieves the desired image by name prefix (possibly cached).
func (p *gcep) getImage(prefix string) (*compute.Image, error) {
	resource, err := p.imageCache.Get(prefix)
	if err != nil {
		return nil, err
	}
	if resource == nil {
		return nil, errors.New("no GCE image with prefix [" + prefix + "] was found")
	}
	return resource.(*compute.Image), nil
}

// prefixMatchImage is a PrefixCallback for prefix-matching images against
// their Names.
func (p *gcep) prefixMatchImage(resource interface{}, prefix string) bool {
	image := resource.(*compute.Image)
	return strings.HasPrefix(image.Name, prefix)
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
	resources, err := p.flavorCache.Resources()
	if err != nil {
		p.Warn("failed to refresh latest flavors", "err", err)
	}

	fmap := make(map[string]*Flavor)
	for key, val := range resources {
		fmap[key] = val.(*Flavor)
	}
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
