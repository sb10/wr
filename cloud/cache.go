// Copyright © 2020 Genome Research Limited
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

// This file contains code for caching resources.

import (
	"sync"
	"time"
)

// CacheDefaultRefresh is a suggested time after which the cache should be
// considered stale and fresh results retrieved.
const CacheDefaultRefresh = 30 * time.Minute

// Cache is used to cache resources that are can only normally be discovered
// with an expensive operation (eg. an API call over the network), but aren't
// expected to change often.
type Cache struct {
	rmap         map[string]interface{}
	crcb         CurrentResourceCallback
	pcb          PrefixCallback
	lastCache    time.Time
	refreshAfter time.Duration
	sync.RWMutex
}

// NewCache returns a new Cache with a Resources() method that will update from
// your CurrentResourceCallback after the given refreshAfter period. (It will
// also update on failing to find a requested resource in the cache.)
func NewCache(callback CurrentResourceCallback, refreshAfter time.Duration) *Cache {
	return &Cache{
		rmap:         make(map[string]interface{}),
		crcb:         callback,
		refreshAfter: refreshAfter,
	}
}

// CurrentResourceCallback should do a live query of your resource and store
// the resource objects in the given map, using the resource ID as key.
type CurrentResourceCallback func(resourceMap map[string]interface{}) error

// PrefixCallback should take one of the resources you mapped during your
// CurrentResourceCallback, and return true if that resource prefix-matches the
// given prefix string. You have the opportunity to prefix-match against any
// property of your resource, not just its ID.
type PrefixCallback func(resource interface{}, prefix string) bool

// SetPrefixCallback sets a PrefixCallback to be used during Get(). Without
// setting one of these, Get() will only do exact matches based on the key you
// mapped each resource to during your CurrentResourceCallback.
func (c *Cache) SetPrefixCallback(cb PrefixCallback) {
	c.Lock()
	defer c.Unlock()
	c.pcb = cb
}

// Get retrieves the desired resource by id from the cache. If it's not in the
// cache, or if it has been longer than refreshAfter time since resources were
// last updated from your callback, will call your CurrentResourceCallback() to
// get any newly added or altered ones. If still not in the cache, returns nil
// and no error.
//
// The only error it will return will be from your own CurrentResourceCallback.
//
// If a PrefixCallback has been set with SetPrefixCallback, id is treated as a
// prefix; exact matches are still preferred, but failing that a random resource
// that your PrefixCallback returns true for when given id will be returned.
func (c *Cache) Get(id string) (interface{}, error) {
	c.RLock()
	var err error
	if time.Since(c.lastCache) > c.refreshAfter {
		c.RUnlock()
		err = c.cacheResources()
	} else {
		c.RUnlock()
	}

	resource := c.getResourceFromCache(id)
	if resource != nil {
		return resource, err
	}

	err2 := c.cacheResources()
	if err == nil && err2 != nil {
		err = err2
	}

	resource = c.getResourceFromCache(id)
	return resource, err
}

// Resources returns all resources. If it has been longer than the refreshAfter
// time since resources were last updated from your callback, your callback will
// be called and fresh results returned.
//
// The only error possible will be from your callback, but stale results will
// still be returned even if the callback errors.
func (c *Cache) Resources() (map[string]interface{}, error) {
	c.RLock()
	var err error
	if time.Since(c.lastCache) > c.refreshAfter {
		c.RUnlock()
		err = c.cacheResources()
		c.RLock()
	}

	// make a copy of our rmap that is safe to hand off
	rmap := make(map[string]interface{})
	for key, val := range c.rmap {
		rmap[key] = val
	}
	c.RUnlock()
	return rmap, err
}

// cacheResources retrieves the current list of resources by calling the
// CurrentResourceCallback and caches them. Old no-longer existent resources are
// kept forever, so we can still query old resources.
func (c *Cache) cacheResources() error {
	c.Lock()
	defer func() {
		c.lastCache = time.Now()
		c.Unlock()
	}()
	return c.crcb(c.rmap)
}

// getResourceFromCache does an exact or prefix match for a resource in the map.
func (c *Cache) getResourceFromCache(id string) interface{} {
	c.RLock()
	defer c.RUnlock()

	// find an exact match
	if resource, found := c.rmap[id]; found {
		return resource
	}

	if c.pcb == nil {
		return nil
	}

	// failing that, find a random prefix match
	for _, resource := range c.rmap {
		if c.pcb(resource, id) {
			return resource
		}
	}
	return nil
}
