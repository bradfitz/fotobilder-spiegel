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
	"xml"
)

var flagBase *string = flag.String("base", "http://www.picpix.com/kelly",
	"e.g. http://www.picpix.com/username (no trailing slash)")

var flagDest *string = flag.String("dest", "/home/bradfitz/backup/picpix/kelly", "Destination backup root")

var flagSloppy *bool = flag.Bool("sloppy", false, "Continue on errors")

var galleryMutex sync.Mutex
var galleryMap map[string]*Gallery = make(map[string]*Gallery)

// Only allow 10 fetches at a time.
var fetchGate chan bool = make(chan bool, 10)

var opsMutex sync.Mutex
var opsInFlight int

var errorMutex sync.Mutex
var errors []string = make([]string, 0)

var galleryPattern *regexp.Regexp = regexp.MustCompile(
	"/gallery/([0-9a-z][0-9a-z][0-9a-z][0-9a-z][0-9a-z][0-9a-z][0-9a-z][0-9a-z])")

func addError(msg string) {
	errorMutex.Lock()
	defer errorMutex.Unlock()
	errors = append(errors, msg)
	if *flagSloppy {
		log.Printf("ERROR: %s", msg)
	} else {
		log.Exitf("ERROR: %s", msg)
	}
}

type Operation interface {
	Done()
}

type NetworkOperation int
type LocalOperation int

func NewLocalOperation() Operation {
	opsMutex.Lock()
	defer opsMutex.Unlock()
	opsInFlight++
	return LocalOperation(0)
}

func NewNetworkOperation() Operation {
	opsMutex.Lock()
	defer opsMutex.Unlock()
	opsInFlight++
	// Buffered channel, will block if too many in-flight.
        fetchGate <- true
	return NetworkOperation(0)
}

func (o LocalOperation) Done() {
	opsMutex.Lock()
        defer opsMutex.Unlock()
	opsInFlight--
}

func (o NetworkOperation) Done() {
	opsMutex.Lock()
        defer opsMutex.Unlock()
	opsInFlight--
	<-fetchGate
}

func AreOperationsInFlight() bool {
	opsMutex.Lock()
        defer opsMutex.Unlock()
	return opsInFlight > 0
}

type Gallery struct {
	key  string
}

func (g *Gallery) GalleryXmlUrl() string {
	return fmt.Sprintf("%s/gallery/%s.xml", *flagBase, g.key)
}

func (g *Gallery) Fetch(op Operation) {
	defer op.Done()

	galXmlFilename := fmt.Sprintf("%s/gallery-%s.xml", *flagDest, g.key)
	fi, statErr := os.Stat(galXmlFilename)
	if statErr != nil || fi.Size == 0 {
		netop := NewNetworkOperation()
		res, _, err := http.Get(g.GalleryXmlUrl())
		if err != nil {
			addError(fmt.Sprintf("Error fetching %s: %v", g.GalleryXmlUrl(), err))
			return
		}
		defer res.Body.Close()

		galXml, err := ioutil.ReadAll(res.Body)
		netop.Done()
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
	
	go fetchPhotosInGallery(galXmlFilename, NewLocalOperation())
}

type DigestInfo struct {
	XMLName xml.Name "digest"
	Type  string "attr"
	Value string "chardata"
}

type MediaFile struct {
	XMLName xml.Name "file"
	Digest DigestInfo
	Mime string
	Width int
	Height int
	Bytes int
	Url string  // the raw URL
}

type MediaSetItem struct {
	XMLName xml.Name "mediaSetItem"
	Title string
	Description string
	InfoURL string  // the xml URL
	File MediaFile
}

type MediaSetItemsWrapper struct {
	XMLName xml.Name "mediaSetItems"
	MediaSetItem []MediaSetItem
}

type LinkedFromSet struct {
	XMLName xml.Name "linkedFrom"
	InfoURL []string   // xml gallery URLs of 'parent' galleries (not a DAG)
}

type LinkedToSet struct {
	XMLName xml.Name "linkedTo"
	InfoURL []string   // xml gallery URLs of 'children' galleries (not a DAG)
}

type MediaSet struct {
	XMLName xml.Name "mediaSet"
	MediaSetItems MediaSetItemsWrapper
	LinkedFrom  LinkedFromSet
	LinkedTo    LinkedToSet
}

func fetchPhotosInGallery(filename string, op Operation) {
	defer op.Done()

	f, err := os.Open(filename, os.O_RDONLY, 0)
	if err != nil {
		addError(fmt.Sprintf("Failed to open %s: %v", filename, err))
		return
	}
	defer f.Close()
	mediaSet := new(MediaSet)
	err = xml.Unmarshal(f, mediaSet)
	if err != nil {
                addError(fmt.Sprintf("Failed to unmarshal %s: %v", filename, err))
                return
        }

	// Learn about new galleries, potentially?
	for _, url := range mediaSet.LinkedFrom.InfoURL {
		noteGallery(url)
	}
	for _, url := range mediaSet.LinkedTo.InfoURL {
		noteGallery(url)
	}

	log.Printf("Parse of %s is: %q", filename, mediaSet)
	for _, item := range mediaSet.MediaSetItems.MediaSetItem {
		log.Printf("   pic: %s", item.InfoURL)
	}
}

func knownGalleries() int {
	galleryMutex.Lock()
	defer galleryMutex.Unlock()
	return len(galleryMap)
}

func noteGallery(keyOrUrl string) {
	var key string

	if len(keyOrUrl) == 8 {
		key = keyOrUrl
	} else {
		matches := galleryPattern.FindAllStringSubmatch(keyOrUrl, -1)
		if len(matches) == 1 {
			key = matches[0][1]
		} else {
			addError("Bogus noted gallery: " + key)
			return
		}
	}
	galleryMutex.Lock()
	defer galleryMutex.Unlock()
	if _, known := galleryMap[key]; known {
		return
	}
	gallery := &Gallery{key}
	galleryMap[key] = gallery

	op := NewLocalOperation()
	go gallery.Fetch(op)
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

	matches := galleryPattern.FindAllStringSubmatch(html, -1)
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

	for AreOperationsInFlight() {
		log.Printf("Operations in-flight.  Waiting.")
		time.Sleep(5 * 1e9)
	}
	log.Printf("Done.")
}