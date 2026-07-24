package gmmu

import (
	"container/list"

	akitavm "github.com/sarchlab/akita/v5/mem/vm"
)

type pwcKey struct {
	PID    akitavm.PID
	Prefix uint64
}

type pwcValue struct {
	ChildTablePAddr uint64
	ReadWrite       bool
	User            bool
	NoExecute       bool
}

type pwcEntry struct {
	key   pwcKey
	value pwcValue
}

type pageWalkCache struct {
	capacity int
	entries  map[pwcKey]*list.Element
	lru      *list.List
}

func newPageWalkCache(capacity int) pageWalkCache {
	return pageWalkCache{
		capacity: capacity,
		entries:  make(map[pwcKey]*list.Element),
		lru:      list.New(),
	}
}

func (c *pageWalkCache) lookup(key pwcKey) (pwcValue, bool) {
	element, ok := c.entries[key]
	if !ok {
		return pwcValue{}, false
	}
	c.lru.MoveToFront(element)
	return element.Value.(pwcEntry).value, true
}

func (c *pageWalkCache) insert(key pwcKey, value pwcValue) (evicted bool) {
	if c.capacity == 0 {
		return false
	}
	if element, ok := c.entries[key]; ok {
		element.Value = pwcEntry{key: key, value: value}
		c.lru.MoveToFront(element)
		return false
	}
	if c.lru.Len() == c.capacity {
		victim := c.lru.Back()
		delete(c.entries, victim.Value.(pwcEntry).key)
		c.lru.Remove(victim)
		evicted = true
	}
	element := c.lru.PushFront(pwcEntry{key: key, value: value})
	c.entries[key] = element
	return evicted
}

func (c *pageWalkCache) invalidatePID(pid akitavm.PID) (invalidated uint64) {
	for key, element := range c.entries {
		if key.PID == pid {
			delete(c.entries, key)
			c.lru.Remove(element)
			invalidated++
		}
	}
	return invalidated
}

func (c *pageWalkCache) reset() (invalidated uint64) {
	invalidated = uint64(c.lru.Len())
	c.entries = make(map[pwcKey]*list.Element)
	c.lru.Init()
	return invalidated
}
