package main

import (
	"flag"
	"fmt"
	"http"
	"io/ioutil"
	"log"
	"regexp"
	"sync"
	"time"
)

var flagBase *string = flag.String("base", "http://www.picpix.com/kelly",
	"e.g. http://www.picpix.com/username (no trailing slash)")

var galleryMutex sync.Mutex
var galleryMap map[string]*Gallery = make(map[string]*Gallery)

// Only allow 10 fetches at a time.
var fetchGate chan bool = make(chan bool, 10)

type Gallery struct {
	key  string
}

func (g *Gallery) Fetch() {
	
}

func knownGalleries() int {
	galleryMutex.Lock()
	defer galleryMutex.Unlock()
	return len(galleryMap)
}

func noteGallery(key string) {
	galleryMutex.Lock()
	defer galleryMutex.Unlock()
	if _, known := galleryMap[key]; known {
		return
	}
	gallery := &Gallery{key}
	galleryMap[key] = gallery
	go gallery.Fetch()
}

func fetchGalleryPage(page int) {
	log.Printf("Fetching gallery page %d", page)
	res, finalUrl, err := http.Get(fmt.Sprintf("%s/?sort=alpha&page=%d",
		*flagBase, page))
	if err != nil {
		log.Exitf("Error fetching gallery page %d: %v", page, err)
	}
	log.Printf("Fetched page %d: %s", page, finalUrl)
	htmlBytes, err := ioutil.ReadAll(res.Body)
	if err != nil {
		log.Exitf("Error reading gallery page %d's HTML: %v", page, err)
	}
	res.Body.Close()

	html := string(htmlBytes)
	log.Printf("read %d bytes", len(html))

	rx := regexp.MustCompile("/gallery/([0-9a-z][0-9a-z][0-9a-z][0-9a-z][0-9a-z][0-9a-z][0-9a-z][0-9a-z])")
	matches := rx.FindAllStringSubmatch(html, -1)
	log.Printf("Matched: %q", matches)
	for _, match := range matches {
		noteGallery(match[1])
	}
}

func main() {
	flag.Parse()
	log.Printf("Starting.")

	page := 1
	for {
		countBefore := knownGalleries()
		fetchGalleryPage(page)
		countAfter := knownGalleries()
		log.Printf("Galleries known: %d", countAfter)
		if countAfter == countBefore {
			log.Printf("No new galleries, stopping.")
			break
		}
		page++
	}

	for len(fetchGate) > 0 {
		log.Printf("Picture downloads in-flight.  Waiting.")
		time.Sleep(5 * 1e9)
	}
	log.Printf("Done.")
}