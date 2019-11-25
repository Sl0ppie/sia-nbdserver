package sia

import (
	"fmt"
	"log"
	"math"
	"os"
	"strings"
	"sync"
	"time"

	"gitlab.com/NebulousLabs/Sia/modules"
	"gitlab.com/NebulousLabs/Sia/node/api/client"

	"github.com/javgh/sia-nbdserver/config"
)

type (
	Backend struct {
		mutex      *sync.Mutex
		cache      *cache
		httpClient *client.Client
	}

	pageAccess struct {
		page      page
		offset    int64
		length    int
		sliceLow  int
		sliceHigh int
	}

	pageIODetails struct {
		file *os.File
	}

	cache struct {
		brain     *cacheBrain
		pageCount int
		pages     []pageIODetails
	}
)

const (
	pageSize              = 64 * 1024 * 1024
	defaultHardMaxCached  = 192
	defaultSoftMaxCached  = 176
	defaultIdleInterval   = 30 * time.Second
	waitInterval          = 5 * time.Second
	defaultDataPieces     = 10
	defaultParityPieces   = 20
	minimumRedundancy     = 2.5
	writeThrottleInterval = 5 * time.Millisecond
	useCachedRenterInfo   = true
)

var (
	siaDaemonAddress = "localhost:9980"
	siaPasswordFile  = config.PrependHomeDirectory(".sia/apipassword")
	siaPathPrefix    = "nbd"
)

func NewBackend(size uint64) (*Backend, error) {
	dataDirectory := config.PrependDataDirectory("")
	log.Printf("Storing cache in %s\n", dataDirectory)
	err := os.MkdirAll(dataDirectory, 0700)
	if err != nil {
		return nil, err
	}

	pageCount := size / pageSize
	if size%pageSize > 0 {
		pageCount += 1
	}

	cacheBrain, err :=
		newCacheBrain(int(pageCount), defaultHardMaxCached, defaultSoftMaxCached, defaultIdleInterval)
	if err != nil {
		return nil, err
	}

	cache := cache{
		brain:     cacheBrain,
		pageCount: int(pageCount),
		pages:     make([]pageIODetails, pageCount),
	}

	siaPassword, err := config.ReadPasswordFile(siaPasswordFile)
	if err != nil {
		return nil, err
	}

	httpClient := client.Client{
		Address:  siaDaemonAddress,
		Password: siaPassword,
	}

	uploadedPages, err := getUploadedPages(&httpClient, false)
	if err != nil {
		return nil, err
	}

	for _, page := range uploadedPages {
		cache.brain.pages[page].state = notCached
	}

	cachedPages := getCachedPages(int(pageCount))
	actions := []action{}
	for _, page := range cachedPages {
		log.Printf("Cache for page %d found - assuming it may contain new data\n", page)
		actions = append(actions, action{
			actionType: openFile,
			page:       page,
		})
		cache.brain.pages[page].state = cachedChanged
		cache.brain.cacheCount += 1
	}

	backend := Backend{
		mutex:      &sync.Mutex{},
		cache:      &cache,
		httpClient: &httpClient,
	}

	_, err = backend.handleActions(actions)
	if err != nil {
		return nil, err
	}

	go func() {
		for {
			time.Sleep(waitInterval)
			_ = backend.maintenance()
		}
	}()

	return &backend, nil
}

func (b *Backend) handleActions(actions []action) (bool, error) {
	for _, action := range actions {
		switch action.actionType {
		case zeroCache:
			log.Printf("Initializing cache for page %d with zeroes\n", action.page)

			buf := make([]byte, pageSize)
			_, err := b.cache.pages[action.page].file.Write(buf)
			if err != nil {
				return false, err
			}
		case deleteCache:
			log.Printf("Deleting cache for page %d\n", action.page)

			cachePath := asCachePath(action.page)
			err := os.Remove(cachePath)
			if err != nil {
				return false, err
			}
		case download:
			log.Printf("Downloading page %d\n", action.page)

			siaPath, err := modules.NewSiaPath(asSiaPath(action.page))
			if err != nil {
				return false, err
			}

			cachePath := asCachePath(action.page)
			_, err = b.httpClient.RenterDownloadFullGet(siaPath, cachePath, false)
			if err != nil {
				return false, err
			}
		case startUpload:
			log.Printf("Uploading page %d\n", action.page)

			siaPath, err := modules.NewSiaPath(asSiaPath(action.page))
			if err != nil {
				return false, err
			}

			cachePath := asCachePath(action.page)
			err = b.httpClient.RenterUploadForcePost(
				cachePath, siaPath, defaultDataPieces, defaultParityPieces, true)
			if err != nil {
				return false, err
			}
		case postponeUpload:
			log.Printf("Postponing upload for page %d\n", action.page)

			siaPath, err := modules.NewSiaPath(asSiaPath(action.page))
			if err != nil {
				return false, err
			}

			err = b.httpClient.RenterDeletePost(siaPath)
			if err != nil {
				return false, err
			}
		case openFile:
			if b.cache.pages[action.page].file != nil {
				panic("file handling is inconsistent")
			}

			file, err := os.OpenFile(asCachePath(action.page), os.O_RDWR|os.O_CREATE, 0600)
			if err != nil {
				return false, err
			}

			b.cache.pages[action.page].file = file
		case closeFile:
			if b.cache.pages[action.page].file == nil {
				panic("file handling is inconsistent")
			}

			err := b.cache.pages[action.page].file.Close()
			if err != nil {
				return false, err
			}

			b.cache.pages[action.page].file = nil
		case waitAndRetry:
			return true, nil
		default:
			panic("unknown action")
		}
	}

	return false, nil
}

func (b *Backend) maintenance() error {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	actions := b.cache.brain.maintenance(time.Now())
	_, err := b.handleActions(actions)
	if err != nil {
		return err
	}

	anyUploading := false
	for i := 0; i < b.cache.brain.pageCount; i++ {
		if b.cache.brain.pages[i].state == cachedUploading {
			anyUploading = true
			break
		}
	}

	if !anyUploading {
		return nil
	}

	uploadedPages, err := getUploadedPages(b.httpClient, true)
	if err != nil {
		return err
	}

	for _, page := range uploadedPages {
		if b.cache.brain.pages[page].state == cachedUploading {
			log.Printf("Upload complete for page %d\n", page)
			b.cache.brain.pages[page].state = cachedUnchanged
		}
	}

	return nil
}

func (b *Backend) ReadAt(buf []byte, offset int64) (int, error) {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	n := 0
	for _, pageAccess := range determinePages(offset, len(buf)) {
		for {
			actions := b.cache.brain.prepareAccess(pageAccess.page, false, time.Now())
			retry, err := b.handleActions(actions)
			if err != nil {
				return n, err
			}

			if !retry {
				break
			} else {
				b.mutex.Unlock()
				time.Sleep(waitInterval)
				b.mutex.Lock()
			}
		}

		partialN, err := b.cache.pages[pageAccess.page].file.ReadAt(
			buf[pageAccess.sliceLow:pageAccess.sliceHigh], pageAccess.offset)
		n += partialN
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func (b *Backend) WriteAt(buf []byte, offset int64) (int, error) {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	writeThrottleLevel := b.cache.brain.cacheCount - (b.cache.brain.softMaxCached + maxConcurrentUploads)
	if writeThrottleLevel >= 0 {
		writeThrottleMultiplier := int64(math.Pow(2, float64(writeThrottleLevel)))
		writeThrottleDuration := time.Duration(writeThrottleMultiplier * int64(writeThrottleInterval))

		b.mutex.Unlock()
		time.Sleep(writeThrottleDuration)
		b.mutex.Lock()
	}

	n := 0
	for _, pageAccess := range determinePages(offset, len(buf)) {
		for {
			actions := b.cache.brain.prepareAccess(pageAccess.page, true, time.Now())
			retry, err := b.handleActions(actions)
			if err != nil {
				return n, err
			}

			if !retry {
				break
			} else {
				b.mutex.Unlock()
				time.Sleep(waitInterval)
				b.mutex.Lock()
			}
		}

		partialN, err := b.cache.pages[pageAccess.page].file.WriteAt(
			buf[pageAccess.sliceLow:pageAccess.sliceHigh], pageAccess.offset)
		n += partialN
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func (b *Backend) Close() error {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	log.Printf("Shutting down\n")
	for {
		actions := b.cache.brain.prepareShutdown()
		retry, err := b.handleActions(actions)
		if err != nil {
			return err
		}

		if !retry {
			break
		} else {
			b.mutex.Unlock()
			time.Sleep(waitInterval)
			b.mutex.Lock()
		}
	}

	return nil
}

func getUploadedPages(httpClient *client.Client, checkRedundancy bool) ([]page, error) {
	pages := []page{}

	renterFiles, err := httpClient.RenterFilesGet(useCachedRenterInfo)
	if err != nil {
		return pages, err
	}

	for _, fileInfo := range renterFiles.Files {
		if !isRelevantSiaPath(fileInfo.SiaPath.String()) {
			continue
		}

		page, err := getPageFromSiaPath(fileInfo.SiaPath.String())
		if err != nil {
			return pages, err
		}

		uploadComplete := fileInfo.Available && fileInfo.Recoverable &&
			(!checkRedundancy || fileInfo.Redundancy >= minimumRedundancy)
		if uploadComplete {
			pages = append(pages, page)
		}
	}

	return pages, nil
}

func getCachedPages(pageCount int) []page {
	pages := []page{}

	for i := 0; i < pageCount; i++ {
		cachePath := asCachePath(page(i))

		if fileCanBeStated(cachePath) {
			pages = append(pages, page(i))
		}
	}

	return pages
}

func fileCanBeStated(name string) bool {
	_, err := os.Stat(name)
	return err == nil
}

func asSiaPath(page page) string {
	return fmt.Sprintf("%s/page%d", siaPathPrefix, page)
}

func asCachePath(page page) string {
	return config.PrependDataDirectory(fmt.Sprintf("page%d", page))
}

func isRelevantSiaPath(siaPath string) bool {
	return strings.HasPrefix(siaPath, fmt.Sprintf("%s/page", siaPathPrefix))
}

func getPageFromSiaPath(siaPath string) (page, error) {
	var page page

	format := fmt.Sprintf("%s/page%%d", siaPathPrefix)
	_, err := fmt.Sscanf(siaPath, format, &page)
	if err != nil {
		return 0, err
	}

	return page, nil
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
