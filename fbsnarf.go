package main

import (
	"flag"
	"fmt"
	"http"
	"io/ioutil"
	"log"
	"os"
	"regexp"
	"sync"
	"time"
)

var flagBase *string = flag.String("base", "http://www.picpix.com/kelly",
	"e.g. http://www.picpix.com/username (no trailing slash)")

var flagDest *string = flag.String("dest", "/home/bradfitz/backup/picpix/kelly", "Destination backup root")

var galleryMutex sync.Mutex
var galleryMap map[string]*Gallery = make(map[string]*Gallery)

// Only allow 10 fetches at a time.
var fetchGate chan bool = make(chan bool, 10)

var errorMutex sync.Mutex
var errors []string = make([]string, 0)

func addError(msg string) {
	errorMutex.Lock()
	defer errorMutex.Unlock()
	errors = append(errors, msg)
	log.Printf("ERROR: %s", msg)
}

func startFetch() {
	// Buffered channel, will block if too many in-flight.
	fetchGate <- true 
}

func endFetch() {
	<-fetchGate
}

type Gallery struct {
	key  string
}

func (g *Gallery) GalleryXmlUrl() string {
	return fmt.Sprintf("%s/gallery/%s.xml", *flagBase, g.key)
}

func (g *Gallery) Fetch() {
	startFetch()
	defer endFetch()

	galXmlFilename := fmt.Sprintf("%s/gallery-%s.xml", *flagDest, g.key)
	fi, statErr := os.Stat(galXmlFilename)
	if statErr != nil || fi.Size == 0 {
		res, _, err := http.Get(g.GalleryXmlUrl())
		if err != nil {
			addError(fmt.Sprintf("Error fetching %s: %v", g.GalleryXmlUrl(), err))
			return
		}
		defer res.Body.Close()

		galXml, err := ioutil.ReadAll(res.Body)
		if err != nil {
			addError(fmt.Sprintf("Error reading XML from %s: %v", g.GalleryXmlUrl(), err))
			return
		}

		err = ioutil.WriteFile(galXmlFilename, galXml, 0600)
 		if err != nil {
			addError(fmt.Sprintf("Error writing file %s: %v", galXmlFilename, err))
			return
		}
	}
	
	fetchPhotosInGallery(galXmlFilename)
}

func fetchPhotosInGallery(filename string) {
	log.Printf("Need to read: %s", filename)
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