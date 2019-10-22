package siaadapter

import (
	"errors"
	"fmt"
	"time"
)

type (
	state int

	page int

	pageDetails struct {
		state           state
		lastAccess      time.Time
		lastWriteAccess time.Time
	}

	pageAccess struct {
		page      page
		offset    int64
		length    int
		sliceLow  int
		sliceHigh int
	}

	Cache struct {
		pageCount     int
		cacheCount    int
		hardMaxCached int
		softMaxCached int
		idleInterval  time.Duration
		pages         []pageDetails
	}

	actionType int

	action struct {
		actionType actionType
		page       page
	}

	SiaAdapter struct{}
)

const (
	pageSize = 64 * 1024 * 1024
)

const (
	zero state = iota
	notCached
	cachedUnchanged
	cachedChanged
	cachedUploading
)

const (
	zeroCache actionType = iota
	deleteCache
	download
	startUpload
	cancelUpload
	waitAndRetry
)

func NewCache(pageCount int, hardMaxCached int, softMaxCached int, idleInterval time.Duration) (*Cache, error) {
	if softMaxCached >= hardMaxCached {
		return nil, errors.New("soft limit needs to be lower than hard limit")
	}

	cache := Cache{
		pageCount:     pageCount,
		cacheCount:    0,
		hardMaxCached: hardMaxCached,
		softMaxCached: softMaxCached,
		idleInterval:  idleInterval,
		pages:         make([]pageDetails, pageCount),
	}
	return &cache, nil
}

func (c *Cache) maintenance(now time.Time) []action {
	actions := []action{}
	hasOldestCachedPage := false
	var oldestCachedPage page
	var oldestAccess time.Time

	for i := 0; i < c.pageCount; i++ {
		if !isCached(c.pages[i].state) {
			continue
		}

		if !hasOldestCachedPage || oldestAccess.After(c.pages[i].lastAccess) {
			hasOldestCachedPage = true
			oldestCachedPage = page(i)
			oldestAccess = c.pages[i].lastAccess
		}

		if c.pages[i].state != cachedChanged {
			continue
		}

		if now.After(c.pages[i].lastWriteAccess.Add(c.idleInterval)) {
			actions = append(actions, action{
				actionType: startUpload,
				page:       page(i),
			})
			c.pages[i].state = cachedUploading
		}
	}

	// Return here if we already have something to do
	// or if we haven't reached our soft limit yet.
	if len(actions) > 0 || c.cacheCount < c.softMaxCached {
		return actions
	}

	switch c.pages[oldestCachedPage].state {
	case cachedUnchanged:
		actions = append(actions, action{
			actionType: deleteCache,
			page:       oldestCachedPage,
		})
		c.pages[oldestCachedPage].state = notCached
		c.cacheCount -= 1
	case cachedChanged:
		actions = append(actions, action{
			actionType: startUpload,
			page:       oldestCachedPage,
		})
		c.pages[oldestCachedPage].state = cachedUploading
	}

	return actions
}

func (c *Cache) prepareAccess(page page, isWrite bool, now time.Time) []action {
	actions := []action{}

	if !isCached(c.pages[page].state) && c.cacheCount >= c.hardMaxCached {
		// need to free up some space first
		actions = c.maintenance(now)
		actions = append(actions, action{
			actionType: waitAndRetry,
		})
		return actions
	}

	switch c.pages[page].state {
	case zero:
		actions = append(actions, action{
			actionType: zeroCache,
			page:       page,
		})
		c.pages[page].state = cachedChanged
		c.cacheCount += 1
	case notCached:
		actions = append(actions, action{
			actionType: download,
			page:       page,
		})
		if isWrite {
			c.pages[page].state = cachedChanged
		} else {
			c.pages[page].state = cachedUnchanged
		}
		c.cacheCount += 1
	case cachedUnchanged:
		if isWrite {
			c.pages[page].state = cachedChanged
		}
	case cachedChanged:
		// no changes
	case cachedUploading:
		if isWrite {
			actions = append(actions, action{
				actionType: cancelUpload,
				page:       page,
			})
			c.pages[page].state = cachedChanged
		}
	default:
		panic("unknown state")
	}

	c.pages[page].lastAccess = now
	if isWrite {
		c.pages[page].lastWriteAccess = now
	}

	return actions
}

func isCached(state state) bool {
	return state == cachedUnchanged || state == cachedChanged || state == cachedUploading
}

func determinePages(offset int64, length int) []pageAccess {
	pageAccesses := []pageAccess{}

	slicePos := 0
	for length > 0 {
		page := page(offset / pageSize)
		pageOffset := offset % pageSize
		remainingPageLength := pageSize - pageOffset
		accessLength := min(int(remainingPageLength), length)

		pageAccesses = append(pageAccesses, pageAccess{
			page:      page,
			offset:    pageOffset,
			length:    accessLength,
			sliceLow:  slicePos,
			sliceHigh: slicePos + accessLength,
		})

		offset += int64(accessLength)
		length -= accessLength
		slicePos += accessLength
	}

	return pageAccesses
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func New() *SiaAdapter {
	siaAdapter := SiaAdapter{}
	return &siaAdapter
}

func (sa *SiaAdapter) ReadAt(b []byte, offset int64) (int, error) {
	//fmt.Println("in ReadAt:", len(b), offset)
	for _, pageAccess := range determinePages(offset, len(b)) {
		fmt.Printf("%dr ", pageAccess.page)
	}
	return len(b), nil
}

func (sa *SiaAdapter) WriteAt(b []byte, offset int64) (int, error) {
	//fmt.Println("in WriteAt:", len(b), offset)
	for _, pageAccess := range determinePages(offset, len(b)) {
		fmt.Printf("%dw ", pageAccess.page)
	}
	return len(b), nil
}
