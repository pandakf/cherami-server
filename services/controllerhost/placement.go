// Copyright (c) 2016 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package controllerhost

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"

	"github.com/uber-common/bark"
	"github.com/uber/cherami-server/common"
	"github.com/uber/cherami-server/distance"
	"github.com/uber/cherami-server/services/controllerhost/load"
)

var errNoHosts = errors.New("Unable to find healthy hosts")
var errNoInputHosts = errors.New("Unable to find healthy input host")
var errNoOutputHosts = errors.New("Unable to find healthy output host")
var errNoStoreHosts = errors.New("Unable to find healthy store hosts")

// Placement is the placement strategy interface for picking hosts
type Placement interface {
	// PickInputHost picks an input host with certain distance from the store hosts
	PickInputHost(storeHosts []*common.HostInfo) (*common.HostInfo, error)
	// PickOutputHost picks an output host with certain distance from the store hosts
	PickOutputHost(storeHosts []*common.HostInfo) (*common.HostInfo, error)
	// PickStoreHosts picks n store hosts with certain distance between store replicas
	PickStoreHosts(count int) ([]*common.HostInfo, error)
}

// DistancePlacement holds the context and distance map
type DistancePlacement struct {
	context *Context
	distMap distance.Map
}

// NewDistancePlacement initializes a new placement topology
func NewDistancePlacement(context *Context) (Placement, error) {
	distMap, err := distance.New(context.appConfig.GetControllerConfig().GetTopologyFile(), context.log)
	// TODO: Add background goroutine to periodically reload the topology file
	return &DistancePlacement{
		context: context,
		distMap: distMap,
	}, err
}

// Helper function to convert host info into resource
func toResources(hosts []*common.HostInfo) []string {
	var resources []string
	for _, host := range hosts {
		resources = append(resources, strings.Split(host.Addr, ":")[0])
	}

	return resources
}

// Helper function to pick hosts based on the predicates
func (p *DistancePlacement) pickHosts(service string, poolHosts, sourceHosts []*common.HostInfo, count int, minDistance, maxDistance uint16) ([]*common.HostInfo, error) {
	if p.distMap == nil {
		return poolHosts[:count], nil
	}

	sourceResources := toResources(sourceHosts)
	poolResources := toResources(poolHosts)

	hostPortMap := make(map[string]int)
	for _, host := range poolHosts {
		if hostPort := strings.Split(host.Addr, ":"); len(hostPort) != 2 {
			p.context.log.WithField("hostPort", hostPort).Panic("Invalid host:port")
		} else if port, err := strconv.Atoi(hostPort[1]); err != nil {
			p.context.log.WithField("port", port).Panic("Invalid port")
		} else {
			hostPortMap[hostPort[0]] = port
		}
	}

	resources, err := p.distMap.FindResources(poolResources, sourceResources, "nic", count, minDistance, maxDistance)
	if err != nil {
		return nil, err
	}

	var hosts []*common.HostInfo
	for _, resource := range resources {
		if port, ok := hostPortMap[resource]; !ok {
			p.context.log.WithField("resource", resource).Panic("Invalid resource")
		} else {
			host, e := p.context.rpm.FindHostForAddr(service, fmt.Sprintf("%s:%d", resource, port))
			if e != nil {
				return nil, e
			}
			hosts = append(hosts, host)
		}
	}
	return hosts, nil
}

// Helper function to pick hosts with fallback predicates
func (p *DistancePlacement) pickHostsWithFallback(service string, minDistance, maxDistance, minFallback, maxFallback uint16, storeHosts []*common.HostInfo) (*common.HostInfo, error) {
	if hosts, err := p.context.rpm.GetHosts(service); err == nil {
		if maxDistance <= minDistance {
			maxDistance = distance.InfiniteDistance
		}
		culledHosts := p.roundRobinCull(hosts, 1, service)
		if len(culledHosts) == 1 {
			return culledHosts[0], nil
		}
	}
	return &common.HostInfo{}, errNoHosts
}

// PickInputHost picks an input host with certain distance from the store hosts
func (p *DistancePlacement) PickInputHost(storeHosts []*common.HostInfo) (*common.HostInfo, error) {
	host, err := p.pickHostsWithFallback(common.InputServiceName,
		p.context.appConfig.GetControllerConfig().GetMinInputToStoreDistance(),
		p.context.appConfig.GetControllerConfig().GetMaxInputToStoreDistance(),
		p.context.appConfig.GetControllerConfig().GetMinInputToStoreFallbackDistance(),
		p.context.appConfig.GetControllerConfig().GetMaxInputToStoreFallbackDistance(),
		storeHosts)
	if err != nil {
		return &common.HostInfo{}, errNoInputHosts
	}
	return host, nil
}

// PickOutputHost picks an output host with certain distance from the store hosts
func (p *DistancePlacement) PickOutputHost(storeHosts []*common.HostInfo) (*common.HostInfo, error) {
	host, err := p.pickHostsWithFallback(common.OutputServiceName,
		p.context.appConfig.GetControllerConfig().GetMinOutputToStoreDistance(),
		p.context.appConfig.GetControllerConfig().GetMaxOutputToStoreDistance(),
		p.context.appConfig.GetControllerConfig().GetMinOutputToStoreFallbackDistance(),
		p.context.appConfig.GetControllerConfig().GetMaxOutputToStoreFallbackDistance(),
		storeHosts)
	if err != nil {
		return &common.HostInfo{}, errNoOutputHosts
	}
	return host, nil
}

// PickStoreHosts picks n store hosts with certain distance between store replicas
func (p *DistancePlacement) PickStoreHosts(count int) ([]*common.HostInfo, error) {

	if storeHosts, err := p.findEligibleStoreHosts(); err == nil {

		if len(storeHosts) < count {
			return nil, errNoHosts
		}

		minDistance := p.context.appConfig.GetControllerConfig().GetMinStoreToStoreDistance()
		maxDistance := p.context.appConfig.GetControllerConfig().GetMaxStoreToStoreDistance()
		if minDistance <= distance.ZeroDistance {
			minDistance = distance.ZeroDistance + 1
		}
		if maxDistance <= minDistance {
			maxDistance = distance.InfiniteDistance
		}
		if hosts, e := p.pickHosts(common.StoreServiceName, storeHosts, nil, count, minDistance, maxDistance); e == nil {
			return hosts, nil
		}
		minFallback := p.context.appConfig.GetControllerConfig().GetMinStoreToStoreFallbackDistance()
		maxFallback := p.context.appConfig.GetControllerConfig().GetMaxStoreToStoreFallbackDistance()
		if minFallback < minDistance || maxFallback > maxDistance {
			if minFallback <= distance.ZeroDistance {
				minFallback = distance.ZeroDistance + 1
			}
			if maxFallback <= minFallback {
				maxFallback = distance.InfiniteDistance
			}
			if hosts, e := p.pickHosts(common.StoreServiceName, storeHosts, nil, count, minFallback, maxFallback); e == nil {
				return hosts, nil
			}
		}
		
		culledStoreHosts := p.roundRobinCull(storeHosts, count, `PickStoreHosts`)
		if len(culledStoreHosts) == count {
			return culledStoreHosts, nil
		}
	}

	return nil, errNoStoreHosts
}

// doesStoreMeetConstraints returns true of the given storehost
// meets all requirements to host a new extent.
func (p *DistancePlacement) doesStoreMeetConstraints(host *common.HostInfo) bool {

	cfgObj, err := p.context.cfgMgr.Get(common.StoreServiceName, "*", host.Sku, host.Name)
	if err != nil {
		return true
	}

	cfg, ok := cfgObj.(StorePlacementConfig)
	if !ok {
		p.context.log.Fatal("Unexpected type mismatch, cfgObj.(StorePlacementConfig) failed !")
	}

	if cfg.AdminStatus != "enabled" {
		p.context.log.WithFields(bark.Fields{
			common.TagHostIP: host.Addr,
			`reason`:         "AdminDisabled"}).Info("Placement ignoring store host")
		return false
	}

	val, err := p.context.loadMetrics.Get(host.UUID, load.EmptyTag, load.RemDiskSpaceBytes, load.OneMinAvg)
	if err != nil {
		return true
	}

	if val <= cfg.MinFreeDiskSpaceBytes {
		p.context.log.WithFields(bark.Fields{
			common.TagHostIP:     host.Addr,
			`freeDiskSpaceBytes`: val,
			`reason`:             "DiskSpaceTooLow"}).Info("Placement ignoring store host")
		return false
	}

	return true
}

// findEligibleStoreHosts gets all store hosts and
// filters them based on AdminStatus. Only returns
// administratively enabled store hosts
func (p *DistancePlacement) findEligibleStoreHosts() ([]*common.HostInfo, error) {

	storeHosts, err := p.context.rpm.GetHosts(common.StoreServiceName)
	if err != nil {
		return nil, err
	}

	result := make([]*common.HostInfo, 0, len(storeHosts))

	for _, h := range storeHosts {
		if p.doesStoreMeetConstraints(h) {
			result = append(result, h)
		}
	}

	// If we didn't find any storehosts, let's say, because they are administratively
	// disabled, then return an error so that the caller can handle appropriately.
	if len(result) == 0 {
		return nil, errNoStoreHosts
	}

	return result, nil
}

var rrMap = map[string]int{}
var rrMapMutex sync.Mutex

func (p *DistancePlacement) roundRobinCull(in []*common.HostInfo, count int, note string) (out []*common.HostInfo) {
	var hi *common.HostInfo
	var min, i int
	out = make([]*common.HostInfo, 0, count)

	ll := func() bark.Logger {
		return p.context.log.WithField(`stressModule`, `roundRobin`).WithField(`note`, note)
	}

	defer rrMapMutex.Unlock()
	rrMapMutex.Lock()

findNextMinimum:
	for {
		min = int(math.MaxInt32)

	findCurrentMinimum:
		for _, hi = range in {
			if hi == nil {
				continue findCurrentMinimum
			}
			if min > rrMap[hi.UUID] {
				min = rrMap[hi.UUID]
			}
			if min == 0 {
				break findCurrentMinimum
			}
		}

	addMinimums:
		for i, hi = range in {
			if hi == nil {
				continue addMinimums
			}
			if rrMap[hi.UUID] == min {
				out = append(out, hi)
				in[i] = nil // Clear this entry so that we can't select it again
				if len(out) == count {
					break addMinimums
				}
			}
		}

		if len(out) == count || min == int(math.MaxInt32) {
			break findNextMinimum
		}
	}

	if len(out) != count {
		ll().
			WithField(`count`, count).
			WithField(`actualCount`, len(out)).
			Error(`failed to build placement team`)
		return make([]*common.HostInfo, 0)
	}

	var s string
	for _, hi = range out {
		s += fmt.Sprintf("%s:%d, ", hi.Name, rrMap[hi.UUID])
		rrMap[hi.UUID]++
	}

	ll().
		WithField(`placement`, s).
		Info(`successfully culled`)

	return out
}
